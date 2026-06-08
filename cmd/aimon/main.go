// Command aimon is a small web service that polls AI subscription / credit
// providers on an interval, persists snapshots to SQLite, and serves a live
// dark-themed dashboard.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
	flag.Parse()

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

	// Build the enabled providers.
	var provs []providers.Provider
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
		log.Info("monitor enabled", "provider", p.ID(), "label", p.Label(), "kind", p.Kind())
	}
	if len(provs) == 0 {
		log.Warn("no monitors enabled — the dashboard will be empty (check config.yaml)")
	}

	pl := poller.New(st, cfg.PollInterval, provs, log)

	srv, err := server.New(cfg, st, pl, oauthMgr, provs, version, log)
	if err != nil {
		log.Error("init server", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Run the poller for the lifetime of the process.
	go pl.Run(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Info("listening", "addr", cfg.Listen, "url", "http://localhost"+cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutdown signal received")

	shutCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		log.Error("graceful shutdown", "err", err)
	}
	log.Info("bye")
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
