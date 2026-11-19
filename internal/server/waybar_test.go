package server

import (
	"encoding/json"
	"fmt"
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
	got := formatWaybar(nil, nil, time.Unix(1_700_000_000, 0))
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
				// 3h of 5h elapsed → linear 60% used; 70 used is over.
				{Name: "5h", Used: 70, ResetAt: now.Add(2 * time.Hour).Unix()},
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
	got := formatWaybar(list, nil, now)
	want := `<span color="#ffffff">C </span><span color="#ff5555">30%</span><span color="#ffffff">/</span><span color="#7dffb3">80%</span>` +
		` · <span color="#ffffff">X </span><span color="#7dffb3">58%</span>` +
		` · <span color="#ffcc66">AI ?</span>`
	if got.Text != want {
		t.Fatalf("text = %q, want %q", got.Text, want)
	}
	if got.Class != waybarClassAuth || got.Alt != waybarClassAuth {
		t.Fatalf("class = %q (want auth: unconnected monitor is worst)", got.Class)
	}
	if got.Percentage == nil || *got.Percentage != 30 {
		t.Fatalf("percentage = %v (tightest healthy remaining over 5h and weekly)", got.Percentage)
	}
	if !strings.Contains(got.Tooltip, "30%/5h  resets 2h  over · linear 40% left") {
		t.Fatalf("tooltip missing 5h span: %q", got.Tooltip)
	}
	if !strings.Contains(got.Tooltip, "80%/1w  resets 4d2h  below · linear 58% left") {
		t.Fatalf("tooltip missing 1w span: %q", got.Tooltip)
	}
	if !strings.Contains(got.Tooltip, "not connected") {
		t.Fatalf("tooltip missing auth: %q", got.Tooltip)
	}
	if len(got.Monitors) != 3 || got.Monitors[0].Class != model.PaceOver || got.Monitors[0].Text != "C 30%/80%" || got.Monitors[1].Text != "X 58%" || got.Monitors[1].Class != model.PaceBelow || got.Monitors[2].Class != waybarClassAuth {
		t.Fatalf("monitors = %+v", got.Monitors)
	}
	if p := got.Monitors[0].Percentage; p == nil || *p != 30 {
		t.Fatalf("claude monitor percentage = %v, want 30", p)
	}
	if len(got.Monitors[0].Spans) != 2 || got.Monitors[0].Spans[0].Name != "5h" || got.Monitors[0].Spans[1].Name != "1w" {
		t.Fatalf("claude waybar spans = %+v, want 5h and 1w", got.Monitors[0].Spans)
	}
}

func TestFormatWaybarPace(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	reset := now.Add(3*24*time.Hour + 12*time.Hour).Unix() // 1w window half elapsed → linear 50%
	mk := func(session, used float64) model.Status {
		return model.Status{
			Provider: "openai", Label: "Codex", Kind: model.KindSubscription, OK: true,
			Used: fptr(used), Total: fptr(100), Balance: fptr(100 - used),
			Spans: []model.Span{
				// 1h of 5h elapsed → linear 20%.
				{Name: "5h", Used: session, ResetAt: now.Add(4 * time.Hour).Unix(), Duration: 5 * 3600},
				{Name: "1w", Used: used, ResetAt: reset, Duration: 7 * 24 * 3600},
			},
		}
	}
	cases := []struct {
		session, used float64
		class         string
		pct           int
	}{
		{10, 40, model.PaceBelow, 60},
		{10, 50, model.PaceAt, 50},
		{10, 55, model.PaceAt, 45},
		{10, 56, model.PaceOver, 44},
		{20, 40, model.PaceAt, 60},   // session on the line, week under
		{30, 40, model.PaceOver, 60}, // session burning fast drives the color
		{70, 40, model.PaceOver, 30}, // and the % when it has less left
	}
	for _, tc := range cases {
		got := formatWaybar([]model.Status{mk(tc.session, tc.used)}, nil, now)
		if got.Class != tc.class {
			t.Fatalf("5h %.0f / 1w %.0f: class = %q, want %q", tc.session, tc.used, got.Class, tc.class)
		}
		if got.Monitors[0].Pace != tc.class {
			t.Fatalf("5h %.0f / 1w %.0f: pace = %q", tc.session, tc.used, got.Monitors[0].Pace)
		}
		if got.Percentage == nil || *got.Percentage != tc.pct {
			t.Fatalf("5h %.0f / 1w %.0f: percentage = %v, want %d", tc.session, tc.used, got.Percentage, tc.pct)
		}
	}
}

func TestFormatWaybarValueColors(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const (
		green = `<span color="#7dffb3">`
		amber = `<span color="#ffcc66">`
		red   = `<span color="#ff5555">`
		end   = `</span>`
		slash = `<span color="#ffffff">/</span>`
	)
	// 5h window with 1h elapsed → linear 20%; 1w window half elapsed → linear 50%.
	session := func(used float64) model.Span {
		return model.Span{Name: "5h", Used: used, ResetAt: now.Add(4 * time.Hour).Unix(), Duration: 5 * 3600}
	}
	week := func(used float64) model.Span {
		return model.Span{Name: "1w", Used: used, ResetAt: now.Add(3*24*time.Hour + 12*time.Hour).Unix(), Duration: 7 * 24 * 3600}
	}
	// Linear 99%: 100 used is within the slack, so the pace alone would be "at".
	weekNearlyOver := model.Span{Name: "1w", Used: 100, ResetAt: now.Add(100 * time.Minute).Unix(), Duration: 7 * 24 * 3600}

	cases := []struct {
		name  string
		spans []model.Span
		text  string
		want  string
	}{
		{"each value has its own pace", []model.Span{session(10), week(50)}, "C 90%/50%", green + "90%" + end + slash + amber + "50%" + end},
		{"short over, long below", []model.Span{session(40), week(10)}, "C 60%/90%", red + "60%" + end + slash + green + "90%" + end},
		{"100% is green", []model.Span{session(0), week(50)}, "C 100%/50%", green + "100%" + end + slash + amber + "50%" + end},
		{"100% with no reset known is green", []model.Span{{Name: "5h", Duration: 5 * 3600}, week(50)}, "C 100%/50%", green + "100%" + end + slash + amber + "50%" + end},
		{"0% is red though pace is at", []model.Span{session(10), weekNearlyOver}, "C 90%/0%", green + "90%" + end + slash + red + "0%" + end},
		{"no pace stays uncolored", []model.Span{{Name: "5h", Used: 30, Duration: 5 * 3600}, week(50)}, "C 70%/50%", "70%" + slash + amber + "50%" + end},
		{"long window only", []model.Span{week(50)}, "C 50%", amber + "50%" + end},
		{"short window only", []model.Span{session(10)}, "C 90%", green + "90%" + end},
		{"monthly counts as long", []model.Span{session(10), {Name: "1mo", Used: 75, Duration: 30 * 24 * 3600}}, "C 90%/25%", green + "90%" + end + slash + "25%"},
		{"group shows least left and worst pace", []model.Span{session(10), week(10), {Name: "1w opus", Used: 50, ResetAt: week(0).ResetAt, Duration: 7 * 24 * 3600}}, "C 90%/50%", green + "90%" + end + slash + amber + "50%" + end},
		{"unwindowed meter is ignored", []model.Span{session(10), {Name: "extra_usage", Used: 99}}, "C 90%", green + "90%" + end},
	}
	for _, tc := range cases {
		got := formatWaybar([]model.Status{{
			Provider: "claude", Label: "Claude", Kind: model.KindSubscription, OK: true, Spans: tc.spans,
		}}, nil, now)
		if got.Monitors[0].Text != tc.text {
			t.Errorf("%s: monitor text = %q, want %q", tc.name, got.Monitors[0].Text, tc.text)
		}
		if want := `<span color="#ffffff">C </span>` + tc.want; got.Text != want {
			t.Errorf("%s: text = %q, want %q", tc.name, got.Text, want)
		}
	}
}

func TestFormatWaybarSessionOnlyFallback(t *testing.T) {
	// No weekly window (Claude Team seat): the session alone drives the bar.
	now := time.Unix(1_700_000_000, 0)
	got := formatWaybar([]model.Status{{
		Provider: "claude", Label: "Claude (Team)", Kind: model.KindSubscription, OK: true,
		Used: fptr(95), Total: fptr(100), Balance: fptr(5),
		Spans: []model.Span{{Name: "5h", Used: 95, ResetAt: now.Add(1 * time.Hour).Unix(), Duration: 5 * 3600}},
	}}, nil, now)
	if got.Monitors[0].Text != "C 5%" {
		t.Fatalf("session-only should show 5h remaining: %q", got.Monitors[0].Text)
	}
	if got.Percentage == nil || *got.Percentage != 5 {
		t.Fatalf("session-only percentage = %v, want 5", got.Percentage)
	}
	if got.Class != model.PaceOver {
		t.Fatalf("session-only class = %q, want over (95%% used, 80%% elapsed)", got.Class)
	}
	if !strings.Contains(got.Tooltip, "5%/5h  resets 1h") {
		t.Fatalf("tooltip = %q", got.Tooltip)
	}
}

func TestBarSpansSessionAndWeekly(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	spans := []model.Span{
		{Name: "5h", Used: 90, Duration: 5 * 3600},
		{Name: "1w", Used: 20, Duration: 7 * 24 * 3600},
		{Name: "extra", Used: 99},
	}
	got := barSpans(spans, now)
	if len(got) != 2 || got[0].Name != "5h" || got[1].Name != "1w" {
		t.Fatalf("barSpans = %+v, want 5h and 1w", got)
	}
	got = barSpans([]model.Span{spans[0], spans[2]}, now)
	if len(got) != 1 || got[0].Name != "5h" {
		t.Fatalf("barSpans session-only = %+v, want only 5h", got)
	}
}

func TestFormatWaybarRolledOverSessionIsFresh(t *testing.T) {
	// A stale snapshot (last-good spans, or the DB-seeded cache) still holds a
	// 5h window that has since reset: it must not pin the bar at 2%.
	now := time.Unix(1_700_000_000, 0)
	got := formatWaybar([]model.Status{{
		Provider: "claude", Label: "Claude (Team)", Kind: model.KindSubscription, OK: true,
		Spans: []model.Span{
			{Name: "5h", Used: 98, ResetAt: now.Add(-time.Minute).Unix(), Duration: 5 * 3600},
			{Name: "1w", Used: 6, ResetAt: now.Add(6 * 24 * time.Hour).Unix(), Duration: 7 * 24 * 3600},
		},
	}}, nil, now)
	if got.Percentage == nil || *got.Percentage != 94 || got.Monitors[0].Text != "C 100%/94%" {
		t.Fatalf("percentage = %v text = %q, want 94 and C 100%%/94%% (5h has reset)", got.Percentage, got.Monitors[0].Text)
	}
	if got.Class == model.PaceOver {
		t.Fatalf("class = %q: a reset session must not score over", got.Class)
	}
	if !strings.Contains(got.Tooltip, "100%/5h") || strings.Contains(got.Tooltip, "2%/5h") {
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
	}, nil, now)
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

func TestMonitorIconsNumbersSecondSeat(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	all := []model.Status{
		{Provider: "claude", Label: "Claude (Max)", Kind: model.KindSubscription, OK: true,
			Spans: []model.Span{{Name: "1w", Used: 20}}},
		{Provider: "xai", Label: "Grok", Kind: model.KindSubscription, OK: true},
		{Provider: "claude", Label: "Claude (Team)", Kind: model.KindSubscription, OK: true,
			Spans: []model.Span{{Name: "1w", Used: 70}}},
		{Provider: "claude", Label: "Claude (Team 2)", Kind: model.KindSubscription, OK: true,
			Spans: []model.Span{{Name: "1w", Used: 50}}},
		{Provider: "claude", Label: "Claude (Team 3)", Kind: model.KindSubscription, OK: true,
			Spans: []model.Span{{Name: "1w", Used: 10}}},
	}
	icons := monitorIcons(all)
	if icons["claude/Claude (Max)"] != "C" || icons["claude/Claude (Team)"] != "C2" ||
		icons["claude/Claude (Team 2)"] != "C3" || icons["claude/Claude (Team 3)"] != "C4" ||
		icons["xai/Grok"] != "X" {
		t.Fatalf("icons = %v", icons)
	}
	got := formatWaybar(all, nil, now)
	if got.Monitors[0].Text != "C 80%" || got.Monitors[2].Text != "C2 30%" ||
		got.Monitors[3].Text != "C3 50%" || got.Monitors[4].Text != "C4 90%" {
		t.Fatalf("monitors = %+v", got.Monitors)
	}
	// Filtering to the team seat alone must keep its C2 token.
	team := filterStatuses(all, "", "claude (team)", "")
	got = formatWaybar(team, icons, now)
	if len(got.Monitors) != 1 || got.Monitors[0].Text != "C2 30%" {
		t.Fatalf("filtered monitors = %+v", got.Monitors)
	}
}

func TestFormatWaybarFullQuotaIsGreen(t *testing.T) {
	// Window just reset: linear expected ≈ 0, used 0 would score "at".
	now := time.Unix(1_700_000_000, 0)
	got := formatWaybar([]model.Status{{
		Provider: "claude", Label: "Claude", Kind: model.KindSubscription, OK: true,
		Used: fptr(0), Total: fptr(100), Balance: fptr(100),
		Spans: []model.Span{{Name: "1w", Used: 0, ResetAt: now.Add(7*24*time.Hour - time.Minute).Unix(), Duration: 7 * 24 * 3600}},
	}}, nil, now)
	if got.Class != model.PaceBelow || got.Monitors[0].Pace != model.PaceBelow {
		t.Fatalf("class = %q pace = %q, want below", got.Class, got.Monitors[0].Pace)
	}
	if !strings.Contains(got.Text, `<span color="#7dffb3">100%</span>`) || got.Monitors[0].Text != "C 100%" {
		t.Fatalf("100%% should be green: %q", got.Text)
	}
	if strings.Contains(got.Text, `color="#ffcc66"`) || strings.Contains(got.Text, `color="#ff5555"`) {
		t.Fatalf("100%% must not be amber or red: %q", got.Text)
	}
	if strings.Contains(got.Tooltip, " at ") {
		t.Fatalf("tooltip still says at for full quota: %q", got.Tooltip)
	}
}

func TestFormatWaybarFullQuotaWithoutWindowIsGreen(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	got := formatWaybar([]model.Status{{
		Provider: "openai", Label: "Codex", Kind: model.KindSubscription, OK: true,
		Balance: fptr(100),
	}}, nil, now)
	if got.Class != model.PaceBelow || got.Monitors[0].Class != model.PaceBelow {
		t.Fatalf("class = %q monitor = %q, want below", got.Class, got.Monitors[0].Class)
	}
	if !strings.Contains(got.Text, `<span color="#7dffb3">100%</span>`) || got.Monitors[0].Text != "AI 100%" {
		t.Fatalf("balance-only 100%% should be green: %q", got.Text)
	}
}

func TestFormatWaybarAuthExpired(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	got := formatWaybar([]model.Status{
		{Provider: "claude", Label: "Claude (Max)", Kind: model.KindSubscription, NeedsAuth: true,
			Error: "auth: refresh token: token endpoint 400: Refresh token expired"},
		{Provider: "claude", Label: "Claude (Team)", Kind: model.KindSubscription, NeedsAuth: true,
			Error: "not connected — click Connect to authorize"},
	}, nil, now)
	if got.Text != `<span color="#ff5555">C!</span> · <span color="#ffcc66">C2 ?</span>` {
		t.Fatalf("text = %q", got.Text)
	}
	if got.Class != waybarClassError {
		t.Fatalf("class = %q", got.Class)
	}
	if got.Monitors[0].Class != waybarClassError || got.Monitors[1].Class != waybarClassAuth {
		t.Fatalf("monitor classes = %q, %q", got.Monitors[0].Class, got.Monitors[1].Class)
	}
	if !strings.Contains(got.Tooltip, "login expired — reconnect") {
		t.Fatalf("tooltip = %q", got.Tooltip)
	}
}

func TestFormatWaybarWindows(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	session := model.Span{Name: "5h", Used: 40, ResetAt: now.Add(4 * time.Hour).Unix(), Duration: 5 * 3600}
	week := model.Span{Name: "1w", Used: 10, ResetAt: now.Add(3*24*time.Hour + 12*time.Hour).Unix(), Duration: 7 * 24 * 3600}
	cases := []struct {
		name string
		st   model.Status
		want []waybarWindow
	}{
		{"short and long", model.Status{Spans: []model.Span{session, week}},
			[]waybarWindow{{"short", 60, model.PaceOver}, {"long", 90, model.PaceBelow}}},
		{"long only", model.Status{Spans: []model.Span{week}}, []waybarWindow{{"long", 90, model.PaceBelow}}},
		{"no window falls back to balance", model.Status{Used: fptr(30), Total: fptr(100), Balance: fptr(70)},
			[]waybarWindow{{"balance", 70, ""}}},
		{"needs auth has none", model.Status{NeedsAuth: true, Spans: []model.Span{week}}, nil},
	}
	for _, tc := range cases {
		st := tc.st
		st.Provider, st.Label, st.Kind, st.OK = "claude", "Claude", model.KindSubscription, true
		got := formatWaybar([]model.Status{st}, nil, now).Monitors[0].Windows
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s: windows = %v, want %v", tc.name, got, tc.want)
		}
	}
	credit := formatWaybar([]model.Status{{Provider: "deepseek", Label: "DS", Kind: model.KindCredit, OK: true, Balance: fptr(5)}}, nil, now)
	if credit.Monitors[0].Windows != nil {
		t.Errorf("credit monitor has windows: %v", credit.Monitors[0].Windows)
	}
}
