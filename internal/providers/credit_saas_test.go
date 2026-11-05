package providers

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/algonode/aimon/internal/config"
)

// fakeAPI answers one path, and only when the key is in the expected header.
func fakeAPI(t *testing.T, path, header, key string, code int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path || r.Header.Get(header) != key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestElevenLabsFetch(t *testing.T) {
	srv := fakeAPI(t, "/v1/user/subscription", "xi-api-key", "k", 200,
		`{"tier":"starter","character_count":1000,"character_limit":30000,"next_character_count_reset_unix":1794000000}`)
	st, err := newElevenLabs(config.Monitor{Label: "ElevenLabs", APIKey: "k", BaseURL: srv.URL}).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.OK || st.Unit != "credits" || st.Plan != "starter" || st.Kind != "credit" {
		t.Fatalf("status = %+v", st)
	}
	if *st.Balance != 29000 || *st.Total != 30000 || *st.Used != 1000 || *st.ResetAt != 1794000000 {
		t.Fatalf("balance %v total %v used %v reset %v", *st.Balance, *st.Total, *st.Used, *st.ResetAt)
	}
}

func TestSerperFetchConvertsCreditsToUSD(t *testing.T) {
	srv := fakeAPI(t, "/account", "X-API-KEY", "k", 200, `{"balance":12500,"rateLimit":50}`)
	st, err := newSerper(config.Monitor{Label: "Serper", APIKey: "k", BaseURL: srv.URL}).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.OK || st.Unit != "USD" || math.Abs(*st.Balance-12.5) > 1e-9 || st.Extra["credits"] != 12500.0 {
		t.Fatalf("status = %+v balance %v", st, *st.Balance)
	}
	srv2 := fakeAPI(t, "/account", "X-API-KEY", "k", 200, `{"balance":1000}`)
	st, err = newSerper(config.Monitor{Label: "Serper", APIKey: "k", BaseURL: srv2.URL, CreditPrice: 0.0003}).Fetch(context.Background())
	if err != nil || math.Abs(*st.Balance-0.3) > 1e-9 {
		t.Fatalf("credit_price: %v err %v", *st.Balance, err)
	}
}

func TestCreditSaaSFailures(t *testing.T) {
	cases := []struct {
		name string
		p    Provider
	}{
		{"elevenlabs no key", newElevenLabs(config.Monitor{})},
		{"serper no key", newSerper(config.Monitor{})},
		{"elevenlabs html 401", newElevenLabs(config.Monitor{APIKey: "k", BaseURL: fakeAPI(t, "/v1/user/subscription", "xi-api-key", "k", 401, "<html>").URL})},
		{"serper html 500", newSerper(config.Monitor{APIKey: "k", BaseURL: fakeAPI(t, "/account", "X-API-KEY", "k", 500, "<html>").URL})},
		{"elevenlabs empty", newElevenLabs(config.Monitor{APIKey: "k", BaseURL: fakeAPI(t, "/v1/user/subscription", "xi-api-key", "k", 200, "{}").URL})},
		{"serper empty", newSerper(config.Monitor{APIKey: "k", BaseURL: fakeAPI(t, "/account", "X-API-KEY", "k", 200, "{}").URL})},
	}
	for _, c := range cases {
		if _, err := c.p.Fetch(context.Background()); err == nil {
			t.Errorf("%s: want an error", c.name)
		}
	}
}
