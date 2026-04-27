// Command middly is a single-binary caching reverse proxy.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"middly/cache"
	"middly/canonical"
	"middly/dashboard"
	"middly/proxy"
)

// defaultRoutes is the out-of-the-box mapping. Override with --routes.
var defaultRoutes = map[string]string{
	"/openai":    "https://api.openai.com",
	"/stripe":    "https://api.stripe.com",
	"/anthropic": "https://api.anthropic.com",
	"/weather":   "https://api.weather.com",
}

func main() {
	var (
		port        int
		dbPath      string
		modeFlag    string
		clearCache  bool
		verbose     bool
		ttl         time.Duration
		includeAuth bool
		routesFlag  string
	)
	flag.IntVar(&port, "port", 8080, "listen port")
	flag.StringVar(&dbPath, "db", defaultDBPath(),
		"sqlite cache path (default: $XDG_STATE_HOME/middly/cache.db, or ./cache.db if it already exists)")
	flag.StringVar(&modeFlag, "mode", "record", "record | replay | passthrough")
	flag.BoolVar(&clearCache, "clear-cache", false, "wipe cache on startup")
	flag.BoolVar(&verbose, "verbose", false, "log every proxied request")
	flag.DurationVar(&ttl, "ttl", 0, "expire cache entries older than this (0 = never)")
	flag.BoolVar(&includeAuth, "include-auth", false, "include Authorization header in canonical hash")
	flag.StringVar(&routesFlag, "routes", "",
		"comma-separated overrides, e.g. /openai=https://api.openai.com,/stripe=https://api.stripe.com")
	flag.Parse()

	// MIDDLY_MODE only takes effect when --mode wasn't explicitly set.
	if env := strings.TrimSpace(os.Getenv("MIDDLY_MODE")); env != "" && modeFlag == "record" {
		modeFlag = env
	}

	mode := proxy.Mode(strings.ToLower(modeFlag))
	switch mode {
	case proxy.ModeRecord, proxy.ModeReplay, proxy.ModePassthrough:
	default:
		fmt.Fprintf(os.Stderr, "invalid mode %q (want record|replay|passthrough)\n", modeFlag)
		os.Exit(2)
	}

	routes, err := mergeRoutes(defaultRoutes, routesFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse routes: %v\n", err)
		os.Exit(2)
	}

	logger := log.New(os.Stderr, "[middly] ", log.LstdFlags|log.Lmicroseconds)

	// Make sure the parent dir exists, whether the user passed an XDG path,
	// a relative path, or something custom.
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			logger.Fatalf("create db dir %q: %v", dir, err)
		}
	}

	store, err := cache.Open(dbPath, ttl)
	if err != nil {
		logger.Fatalf("open cache: %v", err)
	}
	defer store.Close()

	if clearCache {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := store.Clear(ctx); err != nil {
			cancel()
			logger.Fatalf("clear cache: %v", err)
		}
		cancel()
		logger.Printf("cache cleared")
	}

	stats := proxy.NewStats(200)
	opts := canonical.Options{
		IncludeAuth:    includeAuth,
		QueryBlacklist: []string{"_t", "timestamp", "_"},
	}

	srv, err := proxy.New(routes, store, mode, stats, opts, logger)
	if err != nil {
		logger.Fatalf("init proxy: %v", err)
	}

	mux := http.NewServeMux()
	dash := dashboard.New(stats, store, srv)
	dash.Mount(mux)
	mux.Handle("/", srv)

	addr := fmt.Sprintf(":%d", port)
	logger.Printf("listening on http://localhost%s (mode=%s, db=%s, ttl=%s)", addr, mode, dbPath, formatTTL(ttl))
	for prefix, target := range routes {
		logger.Printf("  route %-12s -> %s", prefix, target)
	}
	logger.Printf("dashboard:  http://localhost%s/dashboard", addr)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           withLogging(mux, verbose, logger),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Printf("got %s, shutting down", sig)
	case err := <-errCh:
		if err != nil {
			logger.Fatalf("listen: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}

func mergeRoutes(defaults map[string]string, override string) (map[string]string, error) {
	out := make(map[string]string, len(defaults))
	for k, v := range defaults {
		out[k] = v
	}
	override = strings.TrimSpace(override)
	if override == "" {
		return out, nil
	}
	for _, kv := range strings.Split(override, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("expected prefix=target, got %q", kv)
		}
		out[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	return out, nil
}

func withLogging(h http.Handler, verbose bool, logger *log.Logger) http.Handler {
	if !verbose {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		logger.Printf("%s %s (%s)", r.Method, r.URL.RequestURI(), time.Since(start))
	})
}

func formatTTL(d time.Duration) string {
	if d == 0 {
		return "off"
	}
	return d.String()
}

// defaultDBPath returns a stable on-disk location for the cache. Backwards
// compatible: if the user already has a `./cache.db` in cwd we keep using
// it. Otherwise we use $XDG_STATE_HOME/middly/cache.db (POSIX) or the
// platform-equivalent so the cache survives `cd`-ing into a different
// project directory and re-running middly.
func defaultDBPath() string {
	if _, err := os.Stat("cache.db"); err == nil {
		return "cache.db"
	}
	if state := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); state != "" {
		return filepath.Join(state, "middly", "cache.db")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "middly", "cache.db")
	}
	return "cache.db"
}
