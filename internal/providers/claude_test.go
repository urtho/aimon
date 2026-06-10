package providers

import (
	"encoding/json"
	"math"
	"testing"
	"time"
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
// must not be read as a 0..1 fraction — that made the gauge alternate between
// real values and ~100% used.
func TestParseClaudeUsageSubPercentWindow(t *testing.T) {
	m := mustParse(t, `{
		"five_hour":      {"utilization": 0.8,  "resets_at": "2026-06-08T18:00:00Z"},
		"seven_day":      {"utilization": 32.5, "resets_at": "2026-06-12T00:00:00Z"},
		"seven_day_opus": {"utilization": null, "resets_at": null}
	}`)

	frac, reset, windows, ok := parseClaudeUsage(m)
	if !ok {
		t.Fatal("expected ok")
	}
	if math.Abs(frac-0.325) > 1e-9 {
		t.Fatalf("frac = %v, want 0.325 (seven_day wins; 0.8 means 0.8%%, not 80%%)", frac)
	}
	want, _ := time.Parse(time.RFC3339, "2026-06-12T00:00:00Z")
	if reset != want.Unix() {
		t.Fatalf("reset = %d, want %d (the winning window's resets_at)", reset, want.Unix())
	}
	if got := windows["five_hour"]; got != 0.8 {
		t.Fatalf("windows[five_hour] = %v, want 0.8", got)
	}
	if got := windows["seven_day"]; got != 32.5 {
		t.Fatalf("windows[seven_day] = %v, want 32.5", got)
	}
}

func TestParseClaudeUsageClampsOver100(t *testing.T) {
	m := mustParse(t, `{"five_hour": {"utilization": 104.2}}`)
	frac, _, _, ok := parseClaudeUsage(m)
	if !ok || frac != 1 {
		t.Fatalf("frac = %v ok = %v, want clamped 1", frac, ok)
	}
}

func TestParseClaudeUsageNothingRecognizable(t *testing.T) {
	m := mustParse(t, `{"account": {"email": "x@y.z"}}`)
	if _, _, _, ok := parseClaudeUsage(m); ok {
		t.Fatal("expected ok=false for a payload without utilization")
	}
}

func TestParseClaudeUsageNestedAndString(t *testing.T) {
	// Defensive: windows nested under a list, utilization as a string.
	m := mustParse(t, `{"limits": [{"utilization": "12.5", "resets_at": "2026-06-09T00:00:00Z"}]}`)
	frac, _, _, ok := parseClaudeUsage(m)
	if !ok || math.Abs(frac-0.125) > 1e-9 {
		t.Fatalf("frac = %v ok = %v, want 0.125", frac, ok)
	}
}
