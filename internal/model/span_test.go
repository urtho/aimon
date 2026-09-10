package model

import (
	"testing"
	"time"
)

func TestParseWindowDuration(t *testing.T) {
	cases := map[string]int64{
		"5h":           5 * 3600,
		"1w":           7 * 24 * 3600,
		"1w opus":      7 * 24 * 3600,
		"5h Spark":     5 * 3600,
		"1w review":    7 * 24 * 3600,
		"15m":          15 * 60,
		"1d":           24 * 3600,
		"1mo":          30 * 24 * 3600,
		"30s":          30,
		"extra_usage":  0,
		"nimbus_quill": 0,
		"primary":      0,
		"":             0,
	}
	for name, want := range cases {
		if got := ParseWindowDuration(name); got != want {
			t.Fatalf("ParseWindowDuration(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestSpanPace(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// 5h window, 2.5h left → linear used = 50%.
	sp := Span{Name: "5h", ResetAt: now.Add(2*time.Hour + 30*time.Minute).Unix(), Duration: 5 * 3600}

	lin, ok := sp.LinearUsed(now)
	if !ok || lin < 49.9 || lin > 50.1 {
		t.Fatalf("linear = %v, ok=%v, want 50", lin, ok)
	}

	cases := []struct {
		used  float64
		class string
	}{
		{0, PaceBelow},
		{44, PaceBelow},
		{45, PaceAt},
		{50, PaceAt},
		{55, PaceAt},
		{56, PaceOver},
		{100, PaceOver},
	}
	for _, tc := range cases {
		sp.Used = tc.used
		got, expected, ok := sp.Pace(now)
		if !ok || got != tc.class || expected < 49.9 || expected > 50.1 {
			t.Fatalf("used %.0f: pace=%q expected=%.1f ok=%v, want %q", tc.used, got, expected, ok, tc.class)
		}
	}

	// Name infers duration when Duration is unset (old snapshots).
	named := Span{Name: "1w", Used: 5, ResetAt: now.Add(6 * 24 * time.Hour).Unix()}
	class, expected, ok := named.Pace(now)
	if !ok || class != PaceBelow || expected < 13 || expected > 15 {
		t.Fatalf("inferred 1w: class=%q expected=%.1f ok=%v", class, expected, ok)
	}

	if _, _, ok := (Span{Name: "extra_usage", Used: 80}).Pace(now); ok {
		t.Fatal("span without a window must not score a pace")
	}
}

func TestWorstPace(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	reset5h := now.Add(2*time.Hour + 30*time.Minute).Unix()
	st := Status{OK: true, Spans: []Span{
		{Name: "5h", Used: 50, ResetAt: reset5h, Duration: 5 * 3600},        // at
		{Name: "1w", Used: 90, ResetAt: now.Add(6 * 24 * time.Hour).Unix()}, // over
		{Name: "extra_usage", Used: 99},                                     // ignored
	}}
	got, ok := st.WorstPace(now)
	if !ok || got != PaceOver {
		t.Fatalf("worst = %q ok=%v, want over", got, ok)
	}
	st.OK = false
	if _, ok := st.WorstPace(now); ok {
		t.Fatal("error status must not report a pace")
	}
}
