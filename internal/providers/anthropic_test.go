package providers

import (
	"testing"
	"time"
)

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
