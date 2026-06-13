package providers

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// The Cost API returns amounts in cents as decimal strings; spend must be
// divided by 100 to get USD. Treating "256.30" cents as $256.30 (instead of
// $2.5630) is what made the credit balance wildly negative.
func TestSumReportCents(t *testing.T) {
	const body = `{"data":[
		{"results":[{"currency":"USD","amount":"200.00"},{"currency":"USD","amount":"56.30"}]},
		{"results":[{"currency":"USD","amount":"0.50"}]}
	]}`
	var rep costReport
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cur := "USD"
	cents := sumReportCents(rep, &cur)
	if math.Abs(cents-256.80) > 1e-9 {
		t.Fatalf("cents = %v, want 256.80", cents)
	}
	if usd := cents / 100; math.Abs(usd-2.568) > 1e-9 {
		t.Fatalf("usd = %v, want 2.568 (cents/100)", usd)
	}
}

// The cost report only has daily UTC buckets. A baseline read mid-day must not
// re-count its own day's earlier spend (that's already in the console balance)
// — doing so drove the displayed credit negative.
func TestAnthropicSpendStart(t *testing.T) {
	cases := []struct {
		name string
		asOf time.Time
		want time.Time
	}{
		{
			name: "mid-day baseline skips to the next UTC day",
			asOf: time.Date(2026, 6, 8, 11, 0, 0, 0, time.UTC),
			want: time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "date-only (midnight) baseline includes its day",
			asOf: time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
			want: time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "non-UTC timestamp is normalized to UTC days",
			asOf: time.Date(2026, 6, 8, 23, 30, 0, 0, time.FixedZone("CEST", 2*3600)).UTC(), // 21:30 UTC
			want: time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, tc := range cases {
		a := &anthropic{asOf: tc.asOf, hasCredit: true}
		if got := a.spendStart(); !got.Equal(tc.want) {
			t.Errorf("%s: spendStart() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
