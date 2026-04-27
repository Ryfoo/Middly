package proxy

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"middly/cache"
	"middly/canonical"
)

// upstream that counts hits so we can prove caching short-circuits the network.
func newCountingUpstream(t *testing.T, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "real")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func newServer(t *testing.T, mode Mode, dbPath string, routes map[string]string) (*Server, *cache.Store) {
	t.Helper()
	store, err := cache.Open(dbPath, 0)
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	stats := NewStats(50)
	logger := log.New(io.Discard, "", 0)
	srv, err := New(routes, store, mode, stats, canonical.Options{}, logger)
	if err != nil {
		t.Fatalf("new proxy: %v", err)
	}
	return srv, store
}

func TestRecordThenReplay(t *testing.T) {
	upstream, hits := newCountingUpstream(t, `{"hello":"world"}`)
	dbPath := filepath.Join(t.TempDir(), "cache.db")

	srv, _ := newServer(t, ModeRecord, dbPath, map[string]string{"/openai": upstream.URL})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 1st request -> upstream miss + cache write
	resp1 := mustGet(t, ts.URL+"/openai/v1/ping")
	if resp1.status != http.StatusOK {
		t.Fatalf("status=%d", resp1.status)
	}
	if got, want := resp1.body, `{"hello":"world"}`; got != want {
		t.Fatalf("body=%q want %q", got, want)
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits=%d want 1", hits.Load())
	}

	// Wait for the async cache write to land.
	waitForCacheCount(t, dbPath, 1)

	// 2nd identical request -> cache hit, no upstream call.
	resp2 := mustGet(t, ts.URL+"/openai/v1/ping")
	if resp2.body != `{"hello":"world"}` {
		t.Fatalf("cached body mismatch: %q", resp2.body)
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits=%d want still 1", hits.Load())
	}
	if resp2.headers.Get("X-Cache") != "HIT" {
		t.Fatalf("expected X-Cache: HIT, got %q", resp2.headers.Get("X-Cache"))
	}
	if resp2.headers.Get("X-Upstream") != "real" {
		t.Fatalf("expected upstream headers preserved on replay")
	}
}

func TestReplayModeReturnsErrorOnMiss(t *testing.T) {
	upstream, hits := newCountingUpstream(t, `{}`)
	dbPath := filepath.Join(t.TempDir(), "cache.db")
	srv, _ := newServer(t, ModeReplay, dbPath, map[string]string{"/openai": upstream.URL})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := mustGet(t, ts.URL+"/openai/v1/ping")
	if resp.status != http.StatusBadGateway {
		t.Fatalf("expected 502 on replay miss, got %d", resp.status)
	}
	if hits.Load() != 0 {
		t.Fatalf("replay must not contact upstream, hits=%d", hits.Load())
	}
}

func TestPassthroughModeNeverCaches(t *testing.T) {
	upstream, hits := newCountingUpstream(t, `{}`)
	dbPath := filepath.Join(t.TempDir(), "cache.db")
	srv, store := newServer(t, ModePassthrough, dbPath, map[string]string{"/openai": upstream.URL})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	for i := 0; i < 3; i++ {
		mustGet(t, ts.URL+"/openai/v1/ping")
	}
	if hits.Load() != 3 {
		t.Fatalf("passthrough should hit upstream every time, got %d", hits.Load())
	}
	n, err := store.Count(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("passthrough should not write cache rows, got %d", n)
	}
}

func TestRuntimeModeToggle(t *testing.T) {
	upstream, hits := newCountingUpstream(t, `{"v":1}`)
	dbPath := filepath.Join(t.TempDir(), "cache.db")
	srv, _ := newServer(t, ModeRecord, dbPath, map[string]string{"/u": upstream.URL})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 1) record + miss → upstream
	mustGet(t, ts.URL+"/u/x")
	if hits.Load() != 1 {
		t.Fatalf("hits=%d want 1", hits.Load())
	}
	waitForCacheCount(t, dbPath, 1)

	// 2) flip to replay; cached path still works, upstream untouched
	if err := srv.SetMode(ModeReplay); err != nil {
		t.Fatalf("set replay: %v", err)
	}
	if got := mustGet(t, ts.URL+"/u/x"); got.status != 200 {
		t.Fatalf("expected 200 in replay for cached path, got %d", got.status)
	}
	if hits.Load() != 1 {
		t.Fatalf("replay must not call upstream, hits=%d", hits.Load())
	}
	// Uncached path must 502 in replay
	if got := mustGet(t, ts.URL+"/u/never"); got.status != http.StatusBadGateway {
		t.Fatalf("expected 502 for uncached path in replay, got %d", got.status)
	}

	// 3) flip back to record; new path is fetched & cached again
	if err := srv.SetMode(ModeRecord); err != nil {
		t.Fatalf("set record: %v", err)
	}
	mustGet(t, ts.URL+"/u/y")
	if hits.Load() != 2 {
		t.Fatalf("hits=%d want 2 after toggle back to record", hits.Load())
	}
}

func TestPerRequestModeOverride(t *testing.T) {
	upstream, hits := newCountingUpstream(t, `{"v":1}`)
	dbPath := filepath.Join(t.TempDir(), "cache.db")
	srv, _ := newServer(t, ModeRecord, dbPath, map[string]string{"/u": upstream.URL})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Seed the cache with one entry.
	mustGet(t, ts.URL+"/u/x")
	waitForCacheCount(t, dbPath, 1)
	if hits.Load() != 1 {
		t.Fatalf("setup: hits=%d want 1", hits.Load())
	}

	// Override = passthrough → must hit upstream even though cache exists.
	resp := mustDo(t, "GET", ts.URL+"/u/x", http.Header{"X-Middly-Mode": []string{"passthrough"}})
	if resp.status != 200 {
		t.Fatalf("override passthrough: status=%d", resp.status)
	}
	if hits.Load() != 2 {
		t.Fatalf("override passthrough should bypass cache, hits=%d want 2", hits.Load())
	}

	// Without the header, the cache from step 1 still serves the request
	// (i.e. the passthrough request did NOT poison the cache key, because
	// we strip the header before hashing).
	resp = mustGet(t, ts.URL+"/u/x")
	if resp.headers.Get("X-Cache") != "HIT" {
		t.Fatalf("expected HIT after passthrough override, got %q", resp.headers.Get("X-Cache"))
	}
	if hits.Load() != 2 {
		t.Fatalf("plain request after override should be a HIT, hits=%d", hits.Load())
	}

	// Override = replay on an uncached URL → 502, no upstream call.
	resp = mustDo(t, "GET", ts.URL+"/u/never-cached", http.Header{"X-Middly-Mode": []string{"replay"}})
	if resp.status != http.StatusBadGateway {
		t.Fatalf("override replay on miss: status=%d want 502", resp.status)
	}
	if hits.Load() != 2 {
		t.Fatalf("replay override must not call upstream, hits=%d", hits.Load())
	}
}

func TestNamespaceIsolation(t *testing.T) {
	a, hitsA := newCountingUpstream(t, `{"who":"a"}`)
	b, hitsB := newCountingUpstream(t, `{"who":"b"}`)
	dbPath := filepath.Join(t.TempDir(), "cache.db")
	srv, _ := newServer(t, ModeRecord, dbPath, map[string]string{
		"/a": a.URL,
		"/b": b.URL,
	})
	ts := httptest.NewServer(srv)
	defer ts.Close()

	respA := mustGet(t, ts.URL+"/a/v1/ping")
	respB := mustGet(t, ts.URL+"/b/v1/ping")

	if respA.body == respB.body {
		t.Fatalf("expected different bodies across namespaces")
	}
	if hitsA.Load() != 1 || hitsB.Load() != 1 {
		t.Fatalf("both upstreams should be called once: a=%d b=%d", hitsA.Load(), hitsB.Load())
	}
}

type capturedResp struct {
	status  int
	body    string
	headers http.Header
}

func mustGet(t *testing.T, url string) capturedResp {
	t.Helper()
	return mustDo(t, "GET", url, nil)
}

func mustDo(t *testing.T, method, url string, hdr http.Header) capturedResp {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return capturedResp{status: resp.StatusCode, body: strings.TrimRight(string(body), "\n"), headers: resp.Header}
}

func waitForCacheCount(t *testing.T, dbPath string, want int64) {
	t.Helper()
	store, err := cache.Open(dbPath, 0)
	if err != nil {
		t.Fatalf("reopen cache: %v", err)
	}
	defer store.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n, err := store.Count(context.Background())
		if err == nil && n >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("cache count never reached %d", want)
}

func TestMain(m *testing.M) {
	// quiet sqlite driver init noise
	os.Exit(m.Run())
}
