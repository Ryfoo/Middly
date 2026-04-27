// Package dashboard serves the HTMX-driven UI for inspecting cache traffic.
package dashboard

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"middly/cache"
	"middly/proxy"
)

// Dashboard exposes /dashboard, /dashboard/stats, /dashboard/recent and
// /dashboard/clear (POST).
type Dashboard struct {
	stats *proxy.Stats
	store *cache.Store
	srv   *proxy.Server
	tmpl  *template.Template
}

func New(stats *proxy.Stats, store *cache.Store, srv *proxy.Server) *Dashboard {
	t := template.Must(template.New("layout").Funcs(funcs).Parse(layoutHTML))
	template.Must(t.New("stats").Funcs(funcs).Parse(statsHTML))
	template.Must(t.New("recent").Funcs(funcs).Parse(recentHTML))
	template.Must(t.New("mode").Funcs(funcs).Parse(modeHTML))
	return &Dashboard{stats: stats, store: store, srv: srv, tmpl: t}
}

// Mount registers handlers on mux.
func (d *Dashboard) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/dashboard", d.handleIndex)
	mux.HandleFunc("/dashboard/", d.handleIndex)
	mux.HandleFunc("/dashboard/stats", d.handleStats)
	mux.HandleFunc("/dashboard/recent", d.handleRecent)
	mux.HandleFunc("/dashboard/clear", d.handleClear)
	mux.HandleFunc("/dashboard/mode", d.handleMode)
}

type indexData struct {
	Mode      string
	Routes    []routeView
	CacheRows int64
}

type routeView struct {
	Prefix string
	Target string
}

func (d *Dashboard) handleIndex(w http.ResponseWriter, r *http.Request) {
	count, _ := d.store.Count(r.Context())
	data := indexData{
		Mode:      string(d.srv.Mode()),
		CacheRows: count,
	}
	for _, rt := range d.srv.Routes() {
		data.Routes = append(data.Routes, routeView{Prefix: rt.Prefix, Target: rt.Target.String()})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := d.tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (d *Dashboard) handleStats(w http.ResponseWriter, r *http.Request) {
	snap := d.stats.Snapshot()
	count, _ := d.store.Count(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := d.tmpl.ExecuteTemplate(w, "stats", struct {
		proxy.Snapshot
		Stored int64
	}{snap, count}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (d *Dashboard) handleRecent(w http.ResponseWriter, r *http.Request) {
	snap := d.stats.Snapshot()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := d.tmpl.ExecuteTemplate(w, "recent", snap); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (d *Dashboard) handleMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	mode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode")))
	if mode == "" {
		mode = strings.ToLower(strings.TrimSpace(r.FormValue("mode")))
	}
	if err := d.srv.SetMode(proxy.Mode(mode)); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := d.tmpl.ExecuteTemplate(w, "mode", indexData{Mode: mode}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "mode set to %s\n", mode)
}

func (d *Dashboard) handleClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := d.store.Clear(ctx); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Trigger", "cache-cleared")
	w.WriteHeader(http.StatusNoContent)
}

var funcs = template.FuncMap{
	"shortHash": func(h string) string {
		if len(h) <= 12 {
			return h
		}
		return h[:12]
	},
	"timefmt": func(t time.Time) string {
		return t.Format("15:04:05.000")
	},
	"pct": func(f float64) string {
		return fmt.Sprintf("%.1f%%", f)
	},
	"micros": func(n int64) string {
		switch {
		case n < 1000:
			return fmt.Sprintf("%dµs", n)
		case n < 1_000_000:
			return fmt.Sprintf("%.1fms", float64(n)/1000)
		default:
			return fmt.Sprintf("%.2fs", float64(n)/1_000_000)
		}
	},
}
