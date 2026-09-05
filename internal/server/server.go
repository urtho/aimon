// Package server exposes the dashboard UI, the JSON/SSE API, and the OAuth
// connect endpoints over HTTP.
package server

import (
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/algonode/aimon/internal/config"
	"github.com/algonode/aimon/internal/oauth"
	"github.com/algonode/aimon/internal/poller"
	"github.com/algonode/aimon/internal/providers"
	"github.com/algonode/aimon/internal/store"
	"github.com/algonode/aimon/web"
)

// Server wires the HTTP surface to the poller, store and OAuth manager.
type Server struct {
	cfg     *config.Config
	store   *store.Store
	poller  *poller.Poller
	oauth   *oauth.Manager
	provs   map[string]providers.Provider // keyed by "provider/label"
	tmpl    *template.Template
	log     *slog.Logger
	version string
}

// New constructs a Server. provs is the live provider set (used to resolve
// OAuth configs for connect/disconnect).
func New(cfg *config.Config, st *store.Store, pl *poller.Poller, om *oauth.Manager,
	provs []providers.Provider, version string, log *slog.Logger) (*Server, error) {

	tmpl, err := template.ParseFS(web.FS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	index := make(map[string]providers.Provider, len(provs))
	for _, p := range provs {
		index[p.ID()+"/"+p.Label()] = p
	}
	return &Server{
		cfg: cfg, store: st, poller: pl, oauth: om,
		provs: index, tmpl: tmpl, log: log, version: version,
	}, nil
}

// Handler builds the routed http.Handler (Go 1.22 method+path patterns).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	static, _ := fs.Sub(web.FS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("GET /api/stream", s.handleStream)
	mux.HandleFunc("GET /oauth/start", s.handleOAuthStart)
	mux.HandleFunc("POST /oauth/complete", s.handleOAuthComplete)
	mux.HandleFunc("POST /oauth/disconnect", s.handleOAuthDisconnect)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})

	return s.withLogging(mux)
}

// LoopbackHandler serves Codex/OpenAI's registered OAuth redirect
// (http://localhost:1455/auth/callback). It is a separate listener because
// the public client id only accepts that exact URI — it cannot be rewritten
// to the dashboard port.
func (s *Server) LoopbackHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/callback", s.handleOAuthCallback)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("aimon OpenAI OAuth callback listener\n"))
	})
	return s.withLogging(mux)
}

type pageData struct {
	PollInterval int
	Version      string
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := pageData{
		PollInterval: int(s.cfg.PollInterval / time.Second),
		Version:      s.version,
	}
	if err := s.tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		s.log.Error("render index", "err", err)
	}
}

// withLogging is a tiny access-log + panic-guard middleware.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic in handler", "path", r.URL.Path, "err", rec)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
		// SSE streams are long-lived; only log the quick requests.
		if r.URL.Path != "/api/stream" {
			s.log.Debug("http", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start))
		}
	})
}
