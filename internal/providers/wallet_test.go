package providers

import (
	"context"
	"math"
	"testing"

	"github.com/algonode/aimon/internal/config"
)

func TestWalletAlgorandSpendable(t *testing.T) {
	srv := fakeAPI(t, "/v2/accounts/AAA", "", "", 200, `{"amount":4054501,"min-balance":100000}`)
	st, err := newWallet(config.Monitor{Label: "N", Chain: "algorand", Addresses: []string{"AAA"}, BaseURL: srv.URL,
		WarnBelow: 2, CritBelow: 1}).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Unit != "ALGO" || math.Abs(*st.Balance-3.954501) > 1e-9 || st.Extra["warn_below"] != 2.0 || st.Extra["crit_below"] != 1.0 {
		t.Fatalf("status = %+v balance %v", st, *st.Balance)
	}
}

func TestWalletAlgorandSendsNodeToken(t *testing.T) {
	srv := fakeAPI(t, "/v2/accounts/AAA", "X-Algo-API-Token", "tok", 200, `{"amount":1000000,"min-balance":100000}`)
	st, err := newWallet(config.Monitor{Label: "N", Chain: "algorand", Addresses: []string{"AAA"}, BaseURL: srv.URL,
		APIKey: "tok"}).Fetch(context.Background())
	if err != nil || math.Abs(*st.Balance-0.9) > 1e-9 {
		t.Fatalf("status = %+v err %v", st, err)
	}
}

func TestWalletEVMReportsLowest(t *testing.T) {
	srv := fakeAPI(t, "/", "", "", 200, `[{"id":1,"result":"0xde0b6b3a7640000"},{"id":2,"result":"0x6f05b59d3b20000"}]`)
	st, err := newWallet(config.Monitor{Label: "G", Chain: "evm", Unit: "ETH", Addresses: []string{"0xa", "0xb"},
		BaseURL: srv.URL + "/"}).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Unit != "ETH" || math.Abs(*st.Balance-0.5) > 1e-9 {
		t.Fatalf("status = %+v balance %v", st, *st.Balance)
	}
}

func TestWalletFailures(t *testing.T) {
	short := fakeAPI(t, "/", "", "", 200, `[{"id":1,"result":"0x1"}]`).URL + "/"
	rpcErr := fakeAPI(t, "/", "", "", 200, `[{"id":1,"error":{"message":"x"}}]`).URL + "/"
	down := fakeAPI(t, "/", "", "", 500, "<html>").URL + "/"
	cases := map[string]config.Monitor{
		"no address":  {Chain: "algorand"},
		"bad chain":   {Chain: "x", Addresses: []string{"a"}, BaseURL: down},
		"short batch": {Chain: "evm", Addresses: []string{"a", "b"}, BaseURL: short},
		"rpc error":   {Chain: "evm", Addresses: []string{"a"}, BaseURL: rpcErr},
		"html 500":    {Chain: "evm", Addresses: []string{"a"}, BaseURL: down},
		"algod html":  {Chain: "algorand", Addresses: []string{"a"}, BaseURL: down},
	}
	for name, m := range cases {
		if _, err := newWallet(m).Fetch(context.Background()); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
