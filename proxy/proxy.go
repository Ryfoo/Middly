// Package proxy is the request router + caching reverse proxy. It uses
// httputil.ReverseProxy under the hood so streaming, hop-by-hop header
// stripping and trailer handling come for free; we only intercept the
// response body via ModifyResponse to tee it into the cache.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"middly/cache"
	"middly/canonical"
)

// HeaderModeOverride is the request header clients can set to override the
// global cache mode for a single request. Stripped before canonicalization
// and before forwarding upstream.
const HeaderModeOverride = "X-Middly-Mode"

// Mode controls cache behavior.
type Mode string

const (
	ModeRecord      Mode = "record"
	ModeReplay      Mode = "replay"
	ModePassthrough Mode = "passthrough"
)

// Route ties a URL prefix to an upstream origin.
type Route struct {
	Prefix string
	Target *url.URL
	proxy  *httputil.ReverseProxy
}

// Server is the cache-fronted reverse proxy.
type Server struct {
	routes      []*Route
	routesByLen []*Route
	cache       *cache.Store
	stats       *Stats
	mode        atomic.Pointer[Mode] // hot-path read, lock-free swap via SetMode
	opts        canonical.Options
	log         *log.Logger
}

type contextKey string

const (
	ctxRest   contextKey = "middly.rest"
	ctxHash   contextKey = "middly.hash"
	ctxRoute  contextKey = "middly.route"
	ctxMethod contextKey = "middly.method"
	ctxRecord contextKey = "middly.record"
)

// New builds a Server. routeMap maps a path prefix (e.g. "/openai") to an
// upstream origin (e.g. "https://api.openai.com").
func New(routeMap map[string]string, store *cache.Store, mode Mode, stats *Stats, opts canonical.Options, logger *log.Logger) (*Server, error) {
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{cache: store, stats: stats, opts: opts, log: logger}
	if err := s.SetMode(mode); err != nil {
		return nil, err
	}

	for prefix, target := range routeMap {
		u, err := url.Parse(target)
		if err != nil {
			return nil, fmt.Errorf("bad target for %s: %w", prefix, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("target for %s must include scheme and host: %q", prefix, target)
		}
		prefix = "/" + strings.Trim(prefix, "/")

		upstream := u // capture
		rp := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				rest, _ := req.Context().Value(ctxRest).(string)
				if rest == "" {
					rest = "/"
				}
				req.URL.Scheme = upstream.Scheme
				req.URL.Host = upstream.Host
				req.URL.Path = singleJoiningSlash(upstream.Path, rest)
				req.Host = upstream.Host
				req.Header.Del("X-Forwarded-For")
			},
			ModifyResponse: s.captureResponse,
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
			},
			// Periodic flush keeps SSE / chunked streams responsive.
			FlushInterval: 100 * time.Millisecond,
		}
		s.routes = append(s.routes, &Route{Prefix: prefix, Target: u, proxy: rp})
	}

	s.routesByLen = append([]*Route(nil), s.routes...)
	sort.Slice(s.routesByLen, func(i, j int) bool {
		return len(s.routesByLen[i].Prefix) > len(s.routesByLen[j].Prefix)
	})
	return s, nil
}

// Routes returns the configured routes (read-only view).
func (s *Server) Routes() []*Route { return s.routes }

// Mode returns the current cache mode (lock-free atomic load).
func (s *Server) Mode() Mode {
	if p := s.mode.Load(); p != nil {
		return *p
	}
	return ModeRecord
}

// SetMode atomically swaps the global cache mode. Safe to call from any
// goroutine, including while requests are in flight.
func (s *Server) SetMode(m Mode) error {
	switch m {
	case ModeRecord, ModeReplay, ModePassthrough:
	default:
		return fmt.Errorf("invalid mode %q (want record|replay|passthrough)", m)
	}
	s.mode.Store(&m)
	return nil
}

// resolveMode returns the effective mode for a single request: the
// `X-Middly-Mode` header if set to a valid value, else the global mode.
// The header is stripped from the request so it doesn't affect the
// canonical hash and isn't forwarded upstream.
func (s *Server) resolveMode(r *http.Request) Mode {
	mode := s.Mode()
	if override := r.Header.Get(HeaderModeOverride); override != "" {
		r.Header.Del(HeaderModeOverride)
		switch candidate := Mode(strings.ToLower(override)); candidate {
		case ModeRecord, ModeReplay, ModePassthrough:
			mode = candidate
		}
	}
	return mode
}

// match finds the longest-prefix route for path.
func (s *Server) match(path string) (*Route, string, bool) {
	for _, r := range s.routesByLen {
		if path == r.Prefix || strings.HasPrefix(path, r.Prefix+"/") {
			rest := strings.TrimPrefix(path, r.Prefix)
			if rest == "" {
				rest = "/"
			}
			return r, rest, true
		}
	}
	return nil, "", false
}

// ServeHTTP is the proxy entry point.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	route, rest, ok := s.match(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// Resolve per-request mode override BEFORE canonicalization so the
	// override header itself isn't part of the cache key.
	mode := s.resolveMode(r)

	bodyBytes, err := canonical.ReadAndReplaceBody(r)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	_, hash := canonical.Canonicalize(r.Method, rest, r.URL.RawQuery, r.Header, bodyBytes, canonical.Options{
		QueryBlacklist:  s.opts.QueryBlacklist,
		HeaderWhitelist: s.opts.HeaderWhitelist,
		IncludeAuth:     s.opts.IncludeAuth,
		Namespace:       route.Prefix,
	})

	if mode != ModePassthrough {
		entry, err := s.cache.Get(r.Context(), hash)
		if err != nil {
			s.log.Printf("cache get error: %v", err)
		}
		if entry != nil {
			writeCached(w, entry)
			s.stats.Record(RequestEvent{
				Time: time.Now(), Hash: hash, Method: r.Method,
				Namespace: route.Prefix, Path: rest,
				Status: entry.Status, Hit: true, Mode: string(mode),
				DurMicros: time.Since(start).Microseconds(),
			})
			return
		}
		if mode == ModeReplay {
			http.Error(w, "middly: cache miss in replay mode (hash="+hash+")", http.StatusBadGateway)
			s.stats.Record(RequestEvent{
				Time: time.Now(), Hash: hash, Method: r.Method,
				Namespace: route.Prefix, Path: rest,
				Status: http.StatusBadGateway, Hit: false, Mode: string(mode),
				DurMicros: time.Since(start).Microseconds(),
			})
			return
		}
	}

	// Forward upstream. Stash the routing context so Director and
	// ModifyResponse can see what they need.
	ctx := r.Context()
	ctx = context.WithValue(ctx, ctxRest, rest)
	ctx = context.WithValue(ctx, ctxHash, hash)
	ctx = context.WithValue(ctx, ctxRoute, route)
	ctx = context.WithValue(ctx, ctxMethod, r.Method)
	ctx = context.WithValue(ctx, ctxRecord, mode == ModeRecord)
	r = r.WithContext(ctx)

	sw := &statusWriter{ResponseWriter: w}
	route.proxy.ServeHTTP(sw, r)

	s.stats.Record(RequestEvent{
		Time: time.Now(), Hash: hash, Method: r.Method,
		Namespace: route.Prefix, Path: rest,
		Status: sw.statusOrDefault(), Hit: false, Mode: string(mode),
		DurMicros: time.Since(start).Microseconds(),
	})
}

// captureResponse runs after upstream returns headers but before the body is
// streamed. We wrap the body so each Read also fills a buffer, then on EOF
// we persist the captured bytes to the cache asynchronously.
func (s *Server) captureResponse(resp *http.Response) error {
	record, _ := resp.Request.Context().Value(ctxRecord).(bool)
	if !record {
		return nil
	}
	hash, _ := resp.Request.Context().Value(ctxHash).(string)
	route, _ := resp.Request.Context().Value(ctxRoute).(*Route)
	method, _ := resp.Request.Context().Value(ctxMethod).(string)
	rest, _ := resp.Request.Context().Value(ctxRest).(string)
	if hash == "" || route == nil {
		return nil
	}

	headers := cloneHeaders(resp.Header)
	// Drop framing-specific headers — they get recomputed on replay.
	headers.Del("Transfer-Encoding")
	headers.Del("Content-Length")

	cb := &captureBody{
		rc:  resp.Body,
		buf: new(bytes.Buffer),
		onDone: func(body []byte) {
			ent := &cache.Entry{
				Hash:    hash,
				Status:  resp.StatusCode,
				Headers: headers,
				Body:    body,
			}
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := s.cache.Put(ctx, route.Prefix, method, rest, ent); err != nil {
					s.log.Printf("cache put: %v", err)
				}
			}()
		},
	}
	resp.Body = cb
	return nil
}

// captureBody is the io.TeeReader-style wrapper around an upstream
// response body. Every Read also writes into buf, and on EOF or Close we
// fire onDone exactly once with the captured bytes.
type captureBody struct {
	rc     io.ReadCloser
	buf    *bytes.Buffer
	onDone func([]byte)
	once   sync.Once
}

func (c *captureBody) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		c.buf.Write(p[:n])
	}
	if errors.Is(err, io.EOF) {
		c.fire()
	}
	return n, err
}

func (c *captureBody) Close() error {
	c.fire()
	return c.rc.Close()
}

func (c *captureBody) fire() {
	c.once.Do(func() {
		if c.onDone != nil {
			c.onDone(c.buf.Bytes())
		}
	})
}

// statusWriter remembers the status code written through it and forwards
// Flush so streaming responses still flush through.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status = http.StatusOK
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusWriter) statusOrDefault() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}

func writeCached(w http.ResponseWriter, e *cache.Entry) {
	for k, vs := range e.Headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Cache", "HIT")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(e.Body)))
	w.WriteHeader(e.Status)
	_, _ = w.Write(e.Body)
}

func cloneHeaders(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		out[k] = append([]string(nil), vs...)
	}
	return out
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}
