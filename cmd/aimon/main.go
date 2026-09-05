// Command aimon is a small web service that polls AI subscription / credit
// providers on an interval, persists snapshots to SQLite, and serves a live
// dark-themed dashboard.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/algonode/aimon/internal/config"
	"github.com/algonode/aimon/internal/oauth"
	"github.com/algonode/aimon/internal/poller"
	"github.com/algonode/aimon/internal/providers"
	"github.com/algonode/aimon/internal/server"
	"github.com/algonode/aimon/internal/store"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfgPath := flag.String("config", envOr("AIMON_CONFIG", "config.yaml"), "path to config.yaml")
	healthcheck := flag.String("healthcheck", "", "probe a URL, exit 0 if healthy (for container HEALTHCHECK on scratch)")
	flag.Parse()

	// Self health-check mode: the scratch image has no shell/curl, so the binary
	// probes itself when invoked with -healthcheck.
	if *healthcheck != "" {
		os.Exit(runHealthcheck(*healthcheck))
	}

	log := newLogger()
	log.Info("aimon starting", "version", version, "config", *cfgPath)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}

	st, err := store.Open(cfg.Database)
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	oauthMgr := oauth.New(st)

	// Build the enabled providers. Order is dashboard order.
	var provs []providers.Provider
	var cadence []time.Duration
	for _, m := range cfg.Monitors {
		if !m.IsEnabled() {
			continue
		}
		p, err := providers.Build(m, oauthMgr)
		if err != nil {
			log.Error("build provider", "provider", m.Provider, "label", m.Label, "err", err)
			continue
		}
		provs = append(provs, p)
		cadence = append(cadence, m.PollInterval)
		if m.PollInterval > 0 {
			log.Info("monitor enabled", "provider", p.ID(), "label", p.Label(), "kind", p.Kind(), "poll", m.PollInterval)
		} else {
			log.Info("monitor enabled", "provider", p.ID(), "label", p.Label(), "kind", p.Kind())
		}
	}
	if len(provs) == 0 {
		log.Warn("no monitors enabled — the dashboard will be empty (check config.yaml)")
	}

	pl := poller.New(st, cfg.PollInterval, provs, cadence, log)

	srv, err := server.New(cfg, st, pl, oauthMgr, provs, version, log)
	if err != nil {
		log.Error("init server", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Run the poller for the lifetime of the process.
	go pl.Run(ctx)

	startHTTP := func(name, addr string, handler http.Handler, fatal bool) *http.Server {
		httpSrv := &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			// Derive every request context from the signal context. On SIGINT/SIGTERM
			// this cancels in-flight requests — crucially the long-lived SSE streams,
			// which otherwise keep the connection "active" and stall Shutdown for its
			// full timeout.
			BaseContext: func(net.Listener) context.Context { return ctx },
		}
		go func() {
			log.Info("listening", "name", name, "addr", addr)
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				if !fatal {
					log.Warn("oauth loopback listener failed — paste the callback URL to Connect",
						"addr", addr, "err", err)
					return
				}
				log.Error("http server", "err", err)
				stop()
			}
		}()
		return httpSrv
	}

	httpSrv := startHTTP("dashboard", cfg.Listen, srv.Handler(), true)
	var oauthSrv *http.Server
	if !strings.EqualFold(cfg.OAuthListen, "off") {
		oauthSrv = startHTTP("oauth-loopback", cfg.OAuthListen, srv.LoopbackHandler(), false)
		log.Info("openai oauth callback", "url", "http://localhost:1455/auth/callback")
	}

	<-ctx.Done()
	log.Info("shutdown signal received")

	shutCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		log.Error("graceful shutdown", "err", err)
	}
	if oauthSrv != nil {
		if err := oauthSrv.Shutdown(shutCtx); err != nil {
			log.Error("oauth loopback shutdown", "err", err)
		}
	}
	log.Info("bye")
}

// runHealthcheck performs a one-shot GET and maps the result to an exit code.
func runHealthcheck(url string) int {
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return 0
	}
	fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
	return 1
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch os.Getenv("AIMON_LOG") {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
