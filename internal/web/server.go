// Package web serves the read-only dashboard.
//
// The store handed to this package is opened read-only, so the web process is
// structurally incapable of locking or corrupting the collector's database.
// Combined with running as a separate launchd job, a dashboard fault can never
// cost monitoring data.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/mohammadshamma/monitor-internet/internal/report"
	"github.com/mohammadshamma/monitor-internet/internal/store"
)

//go:embed assets
var assetsFS embed.FS

// Server serves the dashboard and its JSON API.
type Server struct {
	st       *store.Store
	interval time.Duration
	log      *log.Logger
}

// New builds a Server over a read-only store.
func New(st *store.Store, interval time.Duration, lg *log.Logger) *Server {
	return &Server{st: st, interval: interval, log: lg}
}

// Run serves until ctx is cancelled.
func (s *Server) Run(ctx context.Context, host string, port int) error {
	static, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	// Assets come from the embedded FS, so there is no filesystem path to
	// traverse and no loose file that can go missing.
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		data, err := fs.ReadFile(static, "index.html")
		if err != nil {
			http.Error(w, "index missing", http.StatusInternalServerError)
			return
		}
		w.Write(data)
	})

	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/summary", s.handleSummary)
	mux.HandleFunc("GET /api/timeline", s.handleTimeline)
	mux.HandleFunc("GET /api/outages", s.handleOutages)
	mux.HandleFunc("GET /api/targets", s.handleTargets)

	addr := net.JoinHostPort(host, fmt.Sprint(port))
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		s.log.Printf("dashboard on http://%s/", addr)
		if host == "0.0.0.0" {
			s.log.Printf("bound to all interfaces; read-only, no write endpoints")
		}
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// window parses the ?since= parameter, defaulting to 24h.
func (s *Server) window(r *http.Request, def string) (time.Time, time.Time, error) {
	q := r.URL.Query().Get("since")
	if q == "" {
		q = def
	}
	d, err := report.ParseSince(q)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	until := time.Now()
	return until.Add(-d), until, nil
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st, err := report.CurrentStatus(s.st, s.interval)
	if err != nil {
		s.fail(w, err)
		return
	}
	// Never cached: this is the liveness indicator.
	w.Header().Set("Cache-Control", "no-store")
	s.ok(w, st)
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	since, until, err := s.window(r, "30d")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	summary, err := report.BuildSummary(s.st, since, until, s.interval)
	if err != nil {
		s.fail(w, err)
		return
	}
	hist, err := report.HourHistogram(s.st, since, until, s.interval)
	if err != nil {
		s.fail(w, err)
		return
	}
	links, err := report.Links(s.st, since, until)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.ok(w, map[string]interface{}{
		"summary":   summary,
		"hour_isp":  hist,
		"links":     links,
		"generated": time.Now(),
	})
}

func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	since, until, err := s.window(r, "24h")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	buckets := 180
	if v := r.URL.Query().Get("buckets"); v != "" {
		if n, err := fmt.Sscanf(v, "%d", &buckets); n != 1 || err != nil {
			buckets = 180
		}
	}
	if buckets < 10 {
		buckets = 10
	}
	if buckets > 1000 {
		buckets = 1000
	}

	b, err := report.Timeline(s.st, since, until, buckets)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.ok(w, b)
}

func (s *Server) handleOutages(w http.ResponseWriter, r *http.Request) {
	since, until, err := s.window(r, "30d")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	out, err := report.Outages(s.st, since, until)
	if err != nil {
		s.fail(w, err)
		return
	}
	if out == nil {
		out = []report.Outage{}
	}
	s.ok(w, out)
}

func (s *Server) handleTargets(w http.ResponseWriter, r *http.Request) {
	since, until, err := s.window(r, "24h")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	buckets := 120
	if v := r.URL.Query().Get("buckets"); v != "" {
		if n, err := fmt.Sscanf(v, "%d", &buckets); n != 1 || err != nil {
			buckets = 120
		}
	}
	if buckets < 10 {
		buckets = 10
	}
	if buckets > 600 {
		buckets = 600
	}

	series, err := report.TargetsSeries(s.st, since, until, buckets, s.interval)
	if err != nil {
		s.fail(w, err)
		return
	}
	if series == nil {
		series = []report.TargetSeries{}
	}
	s.ok(w, series)
}

func (s *Server) ok(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Printf("warn: encode response: %v", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Printf("error: %v", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
