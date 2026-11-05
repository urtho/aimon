package providers

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/algonode/aimon/internal/config"
)

func TestBraveFetch(t *testing.T) {
	srv := fakeAPI(t, "/counters/braveapi", "", "", 200, strings.Repeat(".", 1002))
	st, err := newBrave(config.Monitor{Label: "Brave", BaseURL: srv.URL + "/counters/braveapi", CreditBalance: 20,
		CreditAsOf: "2026-11-05", CreditBaseCount: 2}).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.OK || st.Unit != "USD" || math.Abs(*st.Balance-15) > 1e-9 || math.Abs(*st.Used-5) > 1e-9 {
		t.Fatalf("status = %+v balance %v used %v", st, *st.Balance, *st.Used)
	}
}

func TestBraveFailures(t *testing.T) {
	ok := fakeAPI(t, "/c", "", "", 200, "..").URL + "/c"
	bad := fakeAPI(t, "/c", "", "", 500, "x").URL + "/c"
	cases := map[string]config.Monitor{
		"no baseline":    {BaseURL: ok},
		"no url":         {CreditBalance: 5},
		"counter 500":    {BaseURL: bad, CreditBalance: 5},
		"counter behind": {BaseURL: ok, CreditBalance: 5, CreditBaseCount: 3},
	}
	for name, m := range cases {
		if _, err := newBrave(m).Fetch(context.Background()); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
