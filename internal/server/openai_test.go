package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/algonode/aimon/internal/config"
	"github.com/algonode/aimon/internal/model"
	"github.com/algonode/aimon/internal/oauth"
	"github.com/algonode/aimon/internal/poller"
	"github.com/algonode/aimon/internal/providers"
	"github.com/algonode/aimon/internal/store"
)

// Exercise provider registration through the poller, persistence, status API,
// and Connect route, so the new fetcher is reachable from the actual dashboard.
func TestOpenAISubscriptionDashboard(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":18000,"reset_at":1900000000}}}`)
	}))
	defer upstream.Close()
	db, err := store.Open(filepath.Join(t.TempDir(), "aimon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	om := oauth.New(db)
	cfg := &config.Config{PollInterval: time.Hour, Monitors: []config.Monitor{
		{Provider: "codex", Label: "Connected", BearerToken: "test-access", UsageURL: upstream.URL},
		{Provider: "openai", Label: "New account", OAuth: true},
	}}
	var provs []providers.Provider
	for _, m := range cfg.Monitors {
		p, err := providers.Build(m, om)
		if err != nil {
			t.Fatal(err)
		}
		provs = append(provs, p)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pl := poller.New(db, cfg.PollInterval, provs, nil, log)
	updates, unsubscribe := pl.Subscribe()
	defer unsubscribe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); pl.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case statuses := <-updates:
		if len(statuses) != 2 {
			t.Fatalf("live update has %d monitors", len(statuses))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no live update from the poller")
	}
	srv, err := New(cfg, db, pl, om, provs, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	handler := srv.Handler()
	get := func(path string, out any) {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: HTTP %d: %s", path, w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
	var status struct {
		Monitors []model.Status `json:"monitors"`
	}
	get("/api/status", &status)
	if len(status.Monitors) != 2 {
		t.Fatalf("status API has %d monitors", len(status.Monitors))
	}
	connected, unconnected := status.Monitors[0], status.Monitors[1]
	if connected.Provider != "openai" || !connected.OK || connected.Balance == nil || *connected.Balance != 75 || len(connected.Spans) != 1 || connected.Spans[0].Name != "5h" {
		t.Fatalf("incorrect dashboard status: %+v", connected)
	}
	if unconnected.OK || !unconnected.NeedsAuth {
		t.Fatalf("missing Connect state: %+v", unconnected)
	}
	var history struct {
		Points []store.Point `json:"points"`
	}
	get("/api/history?provider=openai&label=Connected", &history)
	if len(history.Points) != 1 || history.Points[0].V != 25 {
		t.Fatalf("usage was not persisted: %+v", history.Points)
	}
	var auth struct {
		URL    string `json:"auth_url"`
		State  string `json:"state"`
		Manual bool   `json:"manual"`
	}
	get("/oauth/start?provider=openai&label=New%20account", &auth)
	u, err := url.Parse(auth.URL)
	if err != nil || u.Host != "auth.openai.com" || !auth.Manual || auth.State == "" || u.Query().Get("state") != auth.State {
		t.Fatalf("invalid dashboard Connect response: %+v, %v", auth, err)
	}
}

func TestOpenAILoopbackCallback(t *testing.T) {
	var grants []string
	tokenEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		grants = append(grants, r.PostForm.Get("grant_type"))
		if r.Method != http.MethodPost || r.PostForm.Get("client_id") == "" {
			t.Error("invalid token request")
		}
		if r.PostForm.Get("grant_type") == "authorization_code" {
			if r.PostForm.Get("code") != "test-code" || r.PostForm.Get("redirect_uri") != "http://localhost:1455/auth/callback" || r.PostForm.Get("code_verifier") == "" {
				t.Errorf("incorrect loopback exchange: %v", r.PostForm)
			}
			fmt.Fprint(w, `{"access_token":"loopback-access","refresh_token":"loopback-refresh","expires_in":3600}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer tokenEndpoint.Close()
	usage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer loopback-access" {
			t.Errorf("usage auth %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000}}}`)
	}))
	defer usage.Close()

	db, err := store.Open(filepath.Join(t.TempDir(), "aimon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	om := oauth.New(db)
	cfg := &config.Config{PollInterval: time.Hour, Monitors: []config.Monitor{
		{Provider: "openai", Label: "Codex", OAuth: true, TokenURL: tokenEndpoint.URL, UsageURL: usage.URL},
	}}
	p, err := providers.Build(cfg.Monitors[0], om)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pl := poller.New(db, cfg.PollInterval, []providers.Provider{p}, nil, log)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); pl.Run(ctx) }()
	defer func() { cancel(); <-done }()

	srv, err := New(cfg, db, pl, om, []providers.Provider{p}, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	dash := srv.Handler()
	loop := srv.LoopbackHandler()

	root := httptest.NewRecorder()
	loop.ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	if root.Code != http.StatusOK || !strings.Contains(root.Body.String(), "OAuth callback listener") {
		t.Fatalf("loopback root: HTTP %d %s", root.Code, root.Body.String())
	}

	missing := httptest.NewRecorder()
	dash.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/auth/callback?code=x&state=y", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("dashboard must not serve the Codex loopback path: HTTP %d", missing.Code)
	}

	start := httptest.NewRecorder()
	dash.ServeHTTP(start, httptest.NewRequest(http.MethodGet, "/oauth/start?provider=openai&label=Codex", nil))
	if start.Code != http.StatusOK {
		t.Fatalf("start: HTTP %d %s", start.Code, start.Body.String())
	}
	var auth struct {
		URL    string `json:"auth_url"`
		State  string `json:"state"`
		Manual bool   `json:"manual"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &auth); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(auth.URL)
	if err != nil || !auth.Manual || u.Query().Get("redirect_uri") != "http://localhost:1455/auth/callback" {
		t.Fatalf("authorize URL must keep the registered Codex redirect: %+v, %v", auth, err)
	}

	denied := httptest.NewRecorder()
	loop.ServeHTTP(denied, httptest.NewRequest(http.MethodGet, "/auth/callback?error=access_denied&error_description=user+denied&state="+auth.State, nil))
	if denied.Code != http.StatusBadRequest || !strings.Contains(denied.Body.String(), `data-ok="false"`) || !strings.Contains(denied.Body.String(), "user denied") {
		t.Fatalf("denied: HTTP %d %s", denied.Code, denied.Body.String())
	}

	// Denial consumes the pending flow; Connect must be started again.
	start = httptest.NewRecorder()
	dash.ServeHTTP(start, httptest.NewRequest(http.MethodGet, "/oauth/start?provider=openai&label=Codex", nil))
	if err := json.Unmarshal(start.Body.Bytes(), &auth); err != nil {
		t.Fatal(err)
	}

	cb := httptest.NewRecorder()
	loop.ServeHTTP(cb, httptest.NewRequest(http.MethodGet, "/auth/callback?code=test-code&state="+auth.State, nil))
	if cb.Code != http.StatusOK || !strings.Contains(cb.Body.String(), `data-ok="true"`) || !strings.Contains(cb.Body.String(), "Connected") {
		t.Fatalf("callback: HTTP %d %s", cb.Code, cb.Body.String())
	}
	tok, err := db.GetToken(context.Background(), "openai", "Codex")
	if err != nil || tok.AccessToken != "loopback-access" || tok.RefreshToken != "loopback-refresh" {
		t.Fatalf("stored token: %+v, %v", tok, err)
	}
	if len(grants) != 1 || grants[0] != "authorization_code" {
		t.Fatalf("grants = %v", grants)
	}

	replay := httptest.NewRecorder()
	loop.ServeHTTP(replay, httptest.NewRequest(http.MethodGet, "/auth/callback?code=test-code&state="+auth.State, nil))
	if replay.Code != http.StatusBadRequest || !strings.Contains(replay.Body.String(), `data-ok="false"`) {
		t.Fatalf("replay: HTTP %d %s", replay.Code, replay.Body.String())
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		st := httptest.NewRecorder()
		dash.ServeHTTP(st, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		var status struct {
			Monitors []model.Status `json:"monitors"`
		}
		if err := json.Unmarshal(st.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if len(status.Monitors) == 1 && status.Monitors[0].OK && !status.Monitors[0].NeedsAuth && status.Monitors[0].Balance != nil && *status.Monitors[0].Balance == 90 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dashboard did not pick up loopback login: %+v", status.Monitors)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestOpenAILoopbackCallbackErrors(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "aimon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	om := oauth.New(db)
	cfg := &config.Config{PollInterval: time.Hour, Monitors: []config.Monitor{
		{Provider: "openai", Label: "Codex", OAuth: true},
	}}
	p, err := providers.Build(cfg.Monitors[0], om)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pl := poller.New(db, cfg.PollInterval, []providers.Provider{p}, nil, log)
	srv, err := New(cfg, db, pl, om, []providers.Provider{p}, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	loop := srv.LoopbackHandler()
	for _, path := range []string{
		"/auth/callback",
		"/auth/callback?code=only",
		"/auth/callback?state=only",
		"/auth/callback?code=x&state=unknown",
	} {
		w := httptest.NewRecorder()
		loop.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `data-ok="false"`) {
			t.Fatalf("%s: HTTP %d %s", path, w.Code, w.Body.String())
		}
	}
}
