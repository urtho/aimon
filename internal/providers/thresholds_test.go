package providers

import (
	"context"
	"testing"

	"github.com/algonode/aimon/internal/config"
)

func TestBuildAddsThresholdsToCreditMonitors(t *testing.T) {
	srv := fakeAPI(t, "/account", "X-API-KEY", "k", 200, `{"balance":12500}`)
	p, err := Build(config.Monitor{Provider: "serper", Label: "S", APIKey: "k", BaseURL: srv.URL, WarnBelow: 10, CritBelow: 5}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Extra["warn_below"] != 10.0 || st.Extra["crit_below"] != 5.0 || st.Extra["credits"] != 12500.0 {
		t.Fatalf("extra = %v", st.Extra)
	}
	// No thresholds: the provider is returned unwrapped.
	p, _ = Build(config.Monitor{Provider: "serper", Label: "S", APIKey: "k", BaseURL: srv.URL}, nil)
	if _, wrapped := p.(thresholded); wrapped {
		t.Fatal("wrapped without thresholds")
	}
}

func TestWalletNetwork(t *testing.T) {
	for _, c := range []struct{ chain, network, want string }{
		{"algorand", "", "Algorand"}, {"evm", "", "EVM"}, {"evm", "Base", "Base"},
	} {
		if got := newWallet(config.Monitor{Chain: c.chain, Network: c.network}).network; got != c.want {
			t.Errorf("%s/%q: network %q, want %q", c.chain, c.network, got, c.want)
		}
	}
}
