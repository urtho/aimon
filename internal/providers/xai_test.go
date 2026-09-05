package providers

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseGrokBillingWeeklyCredits(t *testing.T) {
	raw := []byte(`{
		"config": {
			"currentPeriod": {
				"type": "USAGE_PERIOD_TYPE_WEEKLY",
				"start": "2026-08-31T12:05:32.760196+00:00",
				"end": "2026-09-07T12:05:32.760196+00:00"
			},
			"creditUsagePercent": 65.0,
			"onDemandCap": {"val": 0},
			"onDemandUsed": {"val": 0},
			"productUsage": [{"product": "GrokBuild", "usagePercent": 65.0}],
			"isUnifiedBillingUser": true,
			"prepaidBalance": {"val": 0},
			"billingPeriodStart": "2026-08-31T12:05:32.760196+00:00",
			"billingPeriodEnd": "2026-09-07T12:05:32.760196+00:00"
		}
	}`)
	st, err := parseGrokBilling("Grok", raw)
	if err != nil {
		t.Fatal(err)
	}
	if !st.OK || st.Plan != "Grok" || st.Window != "weekly" || st.Unit != "%" {
		t.Fatalf("status meta = ok=%v plan=%q window=%q unit=%q", st.OK, st.Plan, st.Window, st.Unit)
	}
	if st.Used == nil || math.Abs(*st.Used-65) > 1e-9 {
		t.Fatalf("used = %v, want 65", st.Used)
	}
	if st.Balance == nil || math.Abs(*st.Balance-35) > 1e-9 {
		t.Fatalf("balance = %v, want 35 remaining", st.Balance)
	}
	if len(st.Spans) != 1 || st.Spans[0].Name != "1w" || math.Abs(st.Spans[0].Used-65) > 1e-9 {
		t.Fatalf("spans = %+v, want one 1w span at 65%%", st.Spans)
	}
	wantEnd, _ := time.Parse(time.RFC3339Nano, "2026-09-07T12:05:32.760196+00:00")
	if st.ResetAt == nil || *st.ResetAt != wantEnd.Unix() {
		t.Fatalf("reset_at = %v, want %d", st.ResetAt, wantEnd.Unix())
	}
	products, _ := st.Extra["products"].(map[string]float64)
	if products["Build"] != 65 {
		t.Fatalf("products = %#v, want Build=65", st.Extra["products"])
	}
}

func TestParseGrokBillingOmittedPercentIsZero(t *testing.T) {
	// proto3 omits 0.0; a current period still means 0% used.
	raw := []byte(`{
		"config": {
			"currentPeriod": {
				"type": "USAGE_PERIOD_TYPE_WEEKLY",
				"start": "2026-08-31T00:00:00Z",
				"end": "2026-09-07T00:00:00Z"
			},
			"isUnifiedBillingUser": true
		}
	}`)
	st, err := parseGrokBilling("Grok", raw)
	if err != nil {
		t.Fatal(err)
	}
	if st.Used == nil || *st.Used != 0 || st.Balance == nil || *st.Balance != 100 {
		t.Fatalf("used=%v balance=%v, want 0 used / 100 remaining", st.Used, st.Balance)
	}
}

func TestParseGrokBillingLegacyMonthly(t *testing.T) {
	raw := []byte(`{
		"billingCycle": {
			"billingPeriodStart": "2026-04-01T00:00:00Z",
			"billingPeriodEnd": "2026-05-01T00:00:00Z"
		},
		"monthlyLimit": {"val": 2000},
		"usage": {"includedUsed": {"val": 500}, "onDemandUsed": {"val": 0}, "totalUsed": {"val": 500}}
	}`)
	st, err := parseGrokBilling("Grok", raw)
	if err != nil {
		t.Fatal(err)
	}
	if st.Used == nil || math.Abs(*st.Used-25) > 1e-9 {
		t.Fatalf("used = %v, want 25%%", st.Used)
	}
	if len(st.Spans) != 1 || st.Spans[0].Name != "1mo" {
		t.Fatalf("spans = %+v, want 1mo", st.Spans)
	}
}

func TestParseGrokBillingEmptyRejected(t *testing.T) {
	if _, err := parseGrokBilling("Grok", []byte(`{`)); err == nil {
		t.Fatal("want decode error for truncated JSON")
	}
}

func TestParseGrokBillingSubscriptionTier(t *testing.T) {
	raw := []byte(`{
		"config": {
			"creditUsagePercent": 10,
			"currentPeriod": {"type": "USAGE_PERIOD_TYPE_WEEKLY", "end": "2026-09-07T00:00:00Z"}
		},
		"subscription_tier": "X Premium+"
	}`)
	st, err := parseGrokBilling("Grok", raw)
	if err != nil {
		t.Fatal(err)
	}
	if st.Plan != "X Premium+" {
		t.Fatalf("plan = %q, want X Premium+", st.Plan)
	}
}

func TestReadTokenFileGrokNestedAndFlat(t *testing.T) {
	dir := t.TempDir()

	nested := filepath.Join(dir, "grok.json")
	if err := os.WriteFile(nested, []byte(`{
		"https://auth.x.ai::b1a00492-073a-47ea-816f-4c329264a828": {
			"key": "grok-nested-token",
			"refresh_token": "refresh-should-be-ignored",
			"user_id": "u1"
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readTokenFile(nested)
	if err != nil || got != "grok-nested-token" {
		t.Fatalf("nested: got %q err %v, want grok-nested-token", got, err)
	}

	flat := filepath.Join(dir, "hermes.json")
	if err := os.WriteFile(flat, []byte(`{"access_token":"flat-token","refresh_token":"nope"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = readTokenFile(flat)
	if err != nil || got != "flat-token" {
		t.Fatalf("flat: got %q err %v, want flat-token", got, err)
	}
}

func TestIsConnectRPC(t *testing.T) {
	if !isConnectRPC("https://grok.com/grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig") {
		t.Fatal("expected connect RPC URL")
	}
	if isConnectRPC(grokCreditsURL) {
		t.Fatal("credits REST URL must use GET, not Connect POST")
	}
}
