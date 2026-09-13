package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/algonode/aimon/internal/config"
	"github.com/algonode/aimon/internal/model"
	"github.com/algonode/aimon/internal/oauth"
	"github.com/algonode/aimon/internal/poller"
	"github.com/algonode/aimon/internal/store"
)

func fptr(f float64) *float64 { return &f }

func TestFormatWaybarEmpty(t *testing.T) {
	got := formatWaybar(nil, time.Unix(1_700_000_000, 0))
	if got.Text != "aimon" || got.Class != waybarClassEmpty || got.Alt != waybarClassEmpty || got.Tooltip != "no monitors" {
		t.Fatalf("empty: %+v", got)
	}
	if got.Monitors == nil || len(got.Monitors) != 0 {
		t.Fatalf("monitors should be [] not null: %#v", got.Monitors)
	}
}

func TestFormatWaybarSubscriptions(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	list := []model.Status{
		{
			Provider: "claude", Label: "Claude (Max)", Kind: model.KindSubscription, OK: true,
			Plan: "Max", Used: fptr(8), Total: fptr(100), Balance: fptr(92),
			Spans: []model.Span{
				{Name: "5h", Used: 8, ResetAt: now.Add(4*time.Hour + 32*time.Minute).Unix()},
				{Name: "1w", Used: 20, ResetAt: now.Add(4*24*time.Hour + 2*time.Hour).Unix()},
			},
		},
		{
			Provider: "xai", Label: "Grok", Kind: model.KindSubscription, OK: true,
			Plan: "SuperGrok", Used: fptr(42), Total: fptr(100), Balance: fptr(58),
			Spans: []model.Span{
				{Name: "1w", Used: 42, ResetAt: now.Add(3 * 24 * time.Hour).Unix()},
			},
		},
		{
			Provider: "openai", Label: "OpenAI (Codex)", Kind: model.KindSubscription,
			OK: false, NeedsAuth: true, Error: "not connected — click Connect",
		},
	}
	got := formatWaybar(list, now)
	if !strings.Contains(got.Text, "C 80%") || !strings.Contains(got.Text, "X 58%") || !strings.Contains(got.Text, "AI ?") {
		t.Fatalf("text = %q (weekly remaining, not 5h)", got.Text)
	}
	if strings.Contains(got.Text, "C 92%") {
		t.Fatalf("text still uses 5h remaining: %q", got.Text)
	}
	if !strings.Contains(got.Text, `color="#7dffb3"`) || !strings.Contains(got.Text, `color="#ffcc66"`) {
		t.Fatalf("text should pango-color by pace: %q", got.Text)
	}
	if got.Class != waybarClassAuth || got.Alt != waybarClassAuth {
		t.Fatalf("class = %q (want auth: unconnected monitor is worst)", got.Class)
	}
	if got.Percentage == nil || *got.Percentage != 58 {
		t.Fatalf("percentage = %v (tightest healthy weekly remaining)", got.Percentage)
	}
	if strings.Contains(got.Tooltip, "5h") {
		t.Fatalf("tooltip must not include 5h session window: %q", got.Tooltip)
	}
	if !strings.Contains(got.Tooltip, "80%/1w  resets 4d2h  below · linear 58% left") {
		t.Fatalf("tooltip missing 1w span: %q", got.Tooltip)
	}
	if !strings.Contains(got.Tooltip, "not connected") {
		t.Fatalf("tooltip missing auth: %q", got.Tooltip)
	}
	if len(got.Monitors) != 3 || got.Monitors[0].Class != model.PaceBelow || got.Monitors[0].Text != "C 80%" || got.Monitors[1].Text != "X 58%" || got.Monitors[1].Class != model.PaceBelow || got.Monitors[2].Class != waybarClassAuth {
		t.Fatalf("monitors = %+v", got.Monitors)
	}
	if len(got.Monitors[0].Spans) != 1 || got.Monitors[0].Spans[0].Name != "1w" {
		t.Fatalf("claude waybar spans = %+v, want weekly only", got.Monitors[0].Spans)
	}
}

func TestFormatWaybarPace(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	reset := now.Add(3*24*time.Hour + 12*time.Hour).Unix() // 1w window half elapsed → linear 50%
	mk := func(used float64) model.Status {
		return model.Status{
			Provider: "openai", Label: "Codex", Kind: model.KindSubscription, OK: true,
			Used: fptr(used), Total: fptr(100), Balance: fptr(100 - used),
			Spans: []model.Span{
				{Name: "5h", Used: 99, ResetAt: now.Add(time.Hour).Unix(), Duration: 5 * 3600},
				{Name: "1w", Used: used, ResetAt: reset, Duration: 7 * 24 * 3600},
			},
		}
	}
	cases := []struct {
		used  float64
		class string
	}{
		{40, model.PaceBelow},
		{50, model.PaceAt},
		{55, model.PaceAt},
		{56, model.PaceOver},
	}
	for _, tc := range cases {
		got := formatWaybar([]model.Status{mk(tc.used)}, now)
		if got.Class != tc.class {
			t.Fatalf("used %.0f: class = %q, want %q", tc.used, got.Class, tc.class)
		}
		if got.Monitors[0].Pace != tc.class {
			t.Fatalf("used %.0f: pace = %q", tc.used, got.Monitors[0].Pace)
		}
	}
}

func TestFormatWaybarSessionOnly(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	got := formatWaybar([]model.Status{{
		Provider: "openai", Label: "Codex", Kind: model.KindSubscription, OK: true,
		Used: fptr(40), Total: fptr(100), Balance: fptr(60),
		Spans: []model.Span{{Name: "5h", Used: 40, ResetAt: now.Add(2 * time.Hour).Unix(), Duration: 5 * 3600}},
	}}, now)
	if !strings.Contains(got.Text, "AI ok") {
		t.Fatalf("session-only should not show a 5h percent: %q", got.Text)
	}
	if got.Percentage != nil {
		t.Fatalf("session-only percentage = %v, want nil", *got.Percentage)
	}
	if got.Class != waybarClassOK {
		t.Fatalf("session-only class = %q, want ok (no weekly pace)", got.Class)
	}
	if strings.Contains(got.Tooltip, "5h") {
		t.Fatalf("tooltip = %q", got.Tooltip)
	}
}

func TestFormatWaybarCreditsAndErrors(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	got := formatWaybar([]model.Status{
		{Provider: "openrouter", Label: "OpenRouter", Kind: model.KindCredit, OK: true, Balance: fptr(12.4), Unit: "USD"},
		{Provider: "deepseek", Label: "DeepSeek", Kind: model.KindCredit, OK: true, Balance: fptr(3.2), Unit: "CNY"},
		{Provider: "anthropic", Label: "Anthropic API", Kind: model.KindCost, OK: true, Used: fptr(4.5), Unit: "USD", Window: "month-to-date"},
		{Provider: "openai", Label: "Codex", Kind: model.KindSubscription, OK: false, Error: "usage HTTP 500"},
	}, now)
	if got.Text != `OR $12.40 · DS ¥3.20 · AN $4.50 · <span color="#ff5555">AI !</span>` {
		t.Fatalf("text = %q", got.Text)
	}
	if got.Class != waybarClassError {
		t.Fatalf("class = %q", got.Class)
	}
	if got.Percentage != nil {
		t.Fatalf("credits have no aggregate percentage, got %v", *got.Percentage)
	}
	if !strings.Contains(got.Tooltip, "$12.40 left") || !strings.Contains(got.Tooltip, "$4.50 spent  month-to-date") {
		t.Fatalf("tooltip = %q", got.Tooltip)
	}
}

func TestFilterStatuses(t *testing.T) {
	list := []model.Status{
		{Provider: "claude", Label: "Claude (Max)", Kind: model.KindSubscription},
		{Provider: "xai", Label: "Grok", Kind: model.KindSubscription},
		{Provider: "openrouter", Label: "OpenRouter", Kind: model.KindCredit},
	}
	got := filterStatuses(list, "claude,XAI", "", "subscription")
	if len(got) != 2 || got[0].Provider != "claude" || got[1].Provider != "xai" {
		t.Fatalf("provider+kind filter: %+v", got)
	}
	got = filterStatuses(list, "", "grok", "")
	if len(got) != 1 || got[0].Label != "Grok" {
		t.Fatalf("label filter: %+v", got)
	}
	got = filterStatuses(list, "nope", "", "")
	if len(got) != 0 {
		t.Fatalf("empty filter should be empty slice: %+v", got)
	}
}

func TestFmtDur(t *testing.T) {
	cases := map[int64]string{
		0:                "now",
		-5:               "now",
		30:               "1m",
		60:               "1m",
		32 * 60:          "32m",
		4*3600 + 32*60:   "4h32m",
		5 * 3600:         "5h",
		4*86400 + 2*3600: "4d2h",
		3 * 86400:        "3d",
	}
	for sec, want := range cases {
		if got := fmtDur(sec); got != want {
			t.Fatalf("fmtDur(%d) = %q, want %q", sec, got, want)
		}
	}
}

func TestHandleWaybar(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "aimon.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{PollInterval: time.Hour}
	pl := poller.New(db, cfg.PollInterval, nil, nil, log)
	srv, err := New(cfg, db, pl, oauth.New(db), nil, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/waybar?kind=subscription", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type %q", ct)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control %q", w.Header().Get("Cache-Control"))
	}
	var got waybarPayload
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Text != "aimon" || got.Class != waybarClassEmpty || got.Monitors == nil {
		t.Fatalf("payload = %+v", got)
	}
}
