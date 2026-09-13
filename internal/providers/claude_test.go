package providers

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/algonode/aimon/internal/model"
)

func mustParse(t *testing.T, payload string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		t.Fatalf("bad test payload: %v", err)
	}
	return m
}

// Regression: a freshly reset window reporting sub-1% utilization (0.8 == 0.8%)
// must not be read as a 0..1 fraction — that made the card alternate between
// real values and ~100% used.
func TestParseClaudeUsageSubPercentWindow(t *testing.T) {
	m := mustParse(t, `{
		"five_hour":      {"utilization": 0.8,  "resets_at": "2026-06-08T18:00:00Z"},
		"seven_day":      {"utilization": 32.5, "resets_at": "2026-06-12T00:00:00Z"},
		"seven_day_opus": {"utilization": null, "resets_at": null}
	}`)

	spans := parseClaudeUsage(m)
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2 (null-utilization window skipped): %+v", len(spans), spans)
	}
	if spans[0].Name != "5h" || math.Abs(spans[0].Used-0.8) > 1e-9 {
		t.Fatalf("spans[0] = %+v, want 5h at 0.8%% (0.8 means 0.8%%, not 80%%)", spans[0])
	}
	if spans[1].Name != "1w" || math.Abs(spans[1].Used-32.5) > 1e-9 {
		t.Fatalf("spans[1] = %+v, want 1w at 32.5%%", spans[1])
	}
	wantReset, _ := time.Parse(time.RFC3339, "2026-06-12T00:00:00Z")
	if spans[1].ResetAt != wantReset.Unix() {
		t.Fatalf("spans[1].ResetAt = %d, want %d", spans[1].ResetAt, wantReset.Unix())
	}
}

func TestParseClaudeUsageClampsOver100(t *testing.T) {
	m := mustParse(t, `{"five_hour": {"utilization": 104.2}}`)
	spans := parseClaudeUsage(m)
	if len(spans) != 1 || spans[0].Used != 100 {
		t.Fatalf("spans = %+v, want one span clamped to 100", spans)
	}
}

func TestParseClaudeUsageNothingRecognizable(t *testing.T) {
	m := mustParse(t, `{"account": {"email": "x@y.z"}}`)
	if spans := parseClaudeUsage(m); len(spans) != 0 {
		t.Fatalf("spans = %+v, want none for a payload without utilization", spans)
	}
}

func TestParseClaudeUsageNestedAndString(t *testing.T) {
	// Defensive: windows nested under a list, utilization as a string.
	m := mustParse(t, `{"limits": [{"utilization": "12.5", "resets_at": "2026-06-09T00:00:00Z"}]}`)
	spans := parseClaudeUsage(m)
	if len(spans) != 1 || math.Abs(spans[0].Used-12.5) > 1e-9 {
		t.Fatalf("spans = %+v, want one span at 12.5%%", spans)
	}
}

// extra_usage.utilization is extra-credit spend, not plan quota. nimbus_quill
// is an unnamed meter with no window. Neither must become a span or the
// remaining % (Waybar) follows extra credits instead of 5h/1w.
func TestParseClaudeUsageSkipsExtraCredits(t *testing.T) {
	m := mustParse(t, `{
		"five_hour": {"utilization": 0, "resets_at": null, "used_dollars": null},
		"seven_day": {"utilization": 0, "resets_at": "2026-09-20T12:00:00+00:00"},
		"extra_usage": {
			"credits_ever_enabled": true,
			"currency": "EUR",
			"is_enabled": false,
			"monthly_limit": 4000,
			"used_credits": 3053,
			"utilization": 76.325
		},
		"nimbus_quill": {"utilization": 0, "resets_at": null, "used_dollars": null},
		"spend": {"percent": 76, "enabled": false, "utilization": 76}
	}`)
	spans := parseClaudeUsage(m)
	if len(spans) != 2 {
		t.Fatalf("got %d spans: %+v, want 5h and 1w only", len(spans), spans)
	}
	if spans[0].Name != "5h" || spans[0].Used != 0 {
		t.Fatalf("spans[0] = %+v, want 5h at 0%%", spans[0])
	}
	if spans[1].Name != "1w" || spans[1].Used != 0 {
		t.Fatalf("spans[1] = %+v, want 1w at 0%%", spans[1])
	}
	st := model.Status{}
	applySpans(&st, spans)
	if st.Balance == nil || *st.Balance != 100 || st.Used == nil || *st.Used != 0 {
		t.Fatalf("used=%v balance=%v, want 0 / 100", st.Used, st.Balance)
	}
}

func TestApplySpansIgnoresWindowless(t *testing.T) {
	st := model.Status{}
	applySpans(&st, []model.Span{
		{Name: "5h", Used: 0, Duration: 5 * 3600},
		{Name: "1w", Used: 0, ResetAt: 1789905600, Duration: 7 * 24 * 3600},
		{Name: "extra_usage", Used: 76.325},
	})
	if st.Used == nil || *st.Used != 0 || st.Balance == nil || *st.Balance != 100 {
		t.Fatalf("used=%v balance=%v, want 0 / 100 (extra_usage must not drive remaining)", st.Used, st.Balance)
	}
	if len(st.Spans) != 3 {
		t.Fatalf("windowless rows still listed: %+v", st.Spans)
	}
}
