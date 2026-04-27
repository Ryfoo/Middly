// Package canonical builds a deterministic, hashable representation of an
// HTTP request so that identical-by-meaning requests collapse to the same
// SHA-256 hash regardless of header order, JSON key order, or query order.
package canonical

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Options configures which parts of a request participate in the hash.
type Options struct {
	// QueryBlacklist removes specific query parameters from the canonical URL
	// (case-insensitive). Useful for noisy params like cache-busters.
	QueryBlacklist []string
	// HeaderWhitelist, when non-empty, restricts hashed headers to this set
	// (case-insensitive). When empty a sensible default skip-list is used.
	HeaderWhitelist []string
	// IncludeAuth controls whether the Authorization header participates in
	// the hash. Off by default so the same logical request from different
	// developers collides.
	IncludeAuth bool
	// Namespace is the route prefix (e.g. "/openai") prepended to the
	// canonical URL line so two upstreams cannot collide.
	Namespace string
}

func (o Options) queryBlacklist() map[string]struct{} {
	m := make(map[string]struct{}, len(o.QueryBlacklist))
	for _, k := range o.QueryBlacklist {
		m[strings.ToLower(k)] = struct{}{}
	}
	return m
}

func (o Options) headerWhitelist() map[string]struct{} {
	m := make(map[string]struct{}, len(o.HeaderWhitelist))
	for _, k := range o.HeaderWhitelist {
		m[strings.ToLower(k)] = struct{}{}
	}
	return m
}

// Canonicalize builds the canonical string and its SHA-256 hex digest.
// path is the path *after* namespace stripping (e.g. "/v1/chat/completions"),
// rawQuery is the raw query string from the inbound request, headers/body
// come from the original request.
func Canonicalize(method, path, rawQuery string, headers http.Header, body []byte, opts Options) (string, string) {
	var sb strings.Builder

	sb.WriteString(strings.ToUpper(method))
	sb.WriteByte('\n')

	sb.WriteString(opts.Namespace)
	sb.WriteString(path)
	if q := canonicalQuery(rawQuery, opts.queryBlacklist()); q != "" {
		sb.WriteByte('?')
		sb.WriteString(q)
	}
	sb.WriteByte('\n')

	sb.WriteString(canonicalHeaders(headers, opts))
	sb.WriteByte('\n')

	sb.Write(canonicalBody(headers, body))

	canonical := sb.String()
	sum := sha256.Sum256([]byte(canonical))
	return canonical, hex.EncodeToString(sum[:])
}

func canonicalQuery(rawQuery string, blacklist map[string]struct{}) string {
	if rawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return rawQuery
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		if _, blocked := blacklist[strings.ToLower(k)]; blocked {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		vs := append([]string(nil), values[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

// defaultHeaderSkip lists headers that are noisy or hop-specific and would
// destroy cacheability if hashed.
var defaultHeaderSkip = map[string]struct{}{
	"host":              {},
	"user-agent":        {},
	"accept-encoding":   {},
	"connection":        {},
	"content-length":    {},
	"x-forwarded-for":   {},
	"x-forwarded-proto": {},
	"x-forwarded-host":  {},
	"x-real-ip":         {},
	"forwarded":         {},
	"cookie":            {}, // session-bound; opt-in via whitelist if needed
}

func canonicalHeaders(h http.Header, opts Options) string {
	whitelist := opts.headerWhitelist()
	type kv struct{ k, v string }
	var entries []kv
	for k, vs := range h {
		lk := strings.ToLower(k)
		if lk == "authorization" {
			if !opts.IncludeAuth {
				continue
			}
		} else if len(whitelist) > 0 {
			if _, ok := whitelist[lk]; !ok {
				continue
			}
		} else {
			if _, skip := defaultHeaderSkip[lk]; skip {
				continue
			}
		}
		sortedVals := append([]string(nil), vs...)
		sort.Strings(sortedVals)
		entries = append(entries, kv{lk, strings.Join(sortedVals, ",")})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].k < entries[j].k })

	var sb strings.Builder
	for i, e := range entries {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(e.k)
		sb.WriteString(": ")
		sb.WriteString(e.v)
	}
	return sb.String()
}

func canonicalBody(h http.Header, body []byte) []byte {
	if len(body) == 0 {
		return nil
	}
	ct := strings.ToLower(h.Get("Content-Type"))
	if strings.Contains(ct, "application/json") || isLikelyJSON(body) {
		if normalized, err := normalizeJSON(body); err == nil {
			return normalized
		}
	}
	return body
}

func isLikelyJSON(b []byte) bool {
	t := bytes.TrimSpace(b)
	if len(t) == 0 {
		return false
	}
	return t[0] == '{' || t[0] == '['
}

func normalizeJSON(b []byte) ([]byte, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	sorted := sortJSON(v)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(sorted); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func sortJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		sm := newSortedMap(len(t))
		for k, val := range t {
			sm.set(k, sortJSON(val))
		}
		return sm
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = sortJSON(val)
		}
		return out
	default:
		return v
	}
}

// sortedMap marshals JSON object keys in lexicographic order.
type sortedMap struct {
	keys []string
	data map[string]any
}

func newSortedMap(hint int) *sortedMap {
	return &sortedMap{keys: make([]string, 0, hint), data: make(map[string]any, hint)}
}

func (s *sortedMap) set(k string, v any) {
	if _, exists := s.data[k]; !exists {
		s.keys = append(s.keys, k)
	}
	s.data[k] = v
}

func (s *sortedMap) MarshalJSON() ([]byte, error) {
	keys := append([]string(nil), s.keys...)
	sort.Strings(keys)
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := json.Marshal(s.data[k])
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// ReadAndReplaceBody drains req.Body and re-attaches a fresh ReadCloser so
// downstream handlers (the reverse proxy) can still read it.
func ReadAndReplaceBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	return body, nil
}
