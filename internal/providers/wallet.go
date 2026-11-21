package providers

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/algonode/aimon/internal/config"
	"github.com/algonode/aimon/internal/model"
)

// wallet reports an on-chain balance, read from a public node with no key. With
// several addresses (a pool of relayers) it reports the lowest, because any one
// of them running dry is the problem. warn_below and crit_below travel in Extra
// for clients that color the balance: a coin has no meaningful dollar threshold.
type wallet struct {
	label     string
	chain     string
	network   string
	addresses []string
	base      string
	unit      string
	token     string // Algorand: sent as X-Algo-API-Token (Nodely API token; unkeyed calls share a per-IP free quota)
	warn      float64
	crit      float64
}

func newWallet(m config.Monitor) *wallet {
	base := m.BaseURL
	if base == "" && m.Chain == "algorand" {
		base = "https://mainnet-api.algonode.cloud"
	}
	unit := m.Unit
	if unit == "" && m.Chain == "algorand" {
		unit = "ALGO"
	}
	network := m.Network
	if network == "" {
		network = map[string]string{"algorand": "Algorand", "evm": "EVM"}[m.Chain]
	}
	return &wallet{label: m.Label, chain: m.Chain, network: network, addresses: m.Addresses, base: strings.TrimRight(base, "/"),
		unit: unit, token: m.APIKey, warn: m.WarnBelow, crit: m.CritBelow}
}

func (w *wallet) ID() string    { return "wallet" }
func (w *wallet) Label() string { return w.label }
func (w *wallet) Kind() string  { return model.KindCredit }

func (w *wallet) Fetch(ctx context.Context) (model.Status, error) {
	if len(w.addresses) == 0 || w.base == "" {
		return model.Status{}, fmt.Errorf("addresses and base_url are required")
	}
	var balances []float64
	var err error
	switch w.chain {
	case "algorand":
		balances, err = w.algorand(ctx)
	case "evm":
		balances, err = w.evm(ctx)
	default:
		err = fmt.Errorf("unknown chain %q", w.chain)
	}
	if err != nil {
		return model.Status{}, err
	}
	lowest := balances[0]
	for _, b := range balances[1:] {
		if b < lowest {
			lowest = b
		}
	}
	extra := map[string]any{"warn_below": w.warn, "crit_below": w.crit, "network": w.network}
	if len(balances) > 1 {
		extra["note"] = fmt.Sprintf("lowest of %d wallets", len(balances))
	}
	return model.Status{
		Provider: w.ID(),
		Label:    w.label,
		Kind:     w.Kind(),
		OK:       true,
		Unit:     w.unit,
		Balance:  fptr(lowest),
		Extra:    extra,
	}, nil
}

// algorand returns what each account can spend: its amount less the minimum
// balance the protocol locks.
func (w *wallet) algorand(ctx context.Context) ([]float64, error) {
	var headers map[string]string
	if w.token != "" {
		headers = map[string]string{"X-Algo-API-Token": w.token}
	}
	out := make([]float64, 0, len(w.addresses))
	for _, addr := range w.addresses {
		var body struct {
			Amount     *int64 `json:"amount"`
			MinBalance int64  `json:"min-balance"`
		}
		code, raw, err := getJSON(ctx, w.base+"/v2/accounts/"+addr+"?exclude=all", headers, &body)
		if code == 0 {
			return nil, err
		}
		if code != 200 {
			return nil, fmt.Errorf("algod %d: %s", code, snippet(raw, 160))
		}
		if err != nil {
			return nil, err
		}
		if body.Amount == nil {
			return nil, fmt.Errorf("algod returned no amount")
		}
		out = append(out, float64(*body.Amount-body.MinBalance)/1e6)
	}
	return out, nil
}

// evm returns each address's native balance, 18 decimals, in one JSON-RPC batch.
func (w *wallet) evm(ctx context.Context) ([]float64, error) {
	batch := make([]map[string]any, len(w.addresses))
	for i, addr := range w.addresses {
		batch[i] = map[string]any{"jsonrpc": "2.0", "id": i + 1, "method": "eth_getBalance", "params": []string{addr, "latest"}}
	}
	var replies []struct {
		Result string `json:"result"`
	}
	code, raw, err := postJSON(ctx, w.base, nil, batch, &replies)
	if code == 0 {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("rpc %d: %s", code, snippet(raw, 160))
	}
	if err != nil {
		return nil, err
	}
	if len(replies) != len(w.addresses) {
		return nil, fmt.Errorf("rpc returned %d of %d balances", len(replies), len(w.addresses))
	}
	out := make([]float64, 0, len(replies))
	for _, r := range replies {
		wei, ok := new(big.Int).SetString(strings.TrimPrefix(r.Result, "0x"), 16)
		if !ok {
			return nil, fmt.Errorf("rpc returned a bad balance: %s", snippet(raw, 160))
		}
		f, _ := new(big.Float).Quo(new(big.Float).SetInt(wei), big.NewFloat(1e18)).Float64()
		out = append(out, f)
	}
	return out, nil
}
