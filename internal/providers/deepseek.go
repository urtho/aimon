package providers

import (
	"context"
	"fmt"
	"strconv"

	"github.com/algonode/aimon/internal/config"
	"github.com/algonode/aimon/internal/model"
)

// deepSeek reports the prepaid balance from https://api.deepseek.com/user/balance.
type deepSeek struct {
	label  string
	apiKey string
	base   string
}

func newDeepSeek(m config.Monitor) *deepSeek {
	base := m.BaseURL
	if base == "" {
		base = "https://api.deepseek.com"
	}
	return &deepSeek{label: m.Label, apiKey: m.APIKey, base: base}
}

func (d *deepSeek) ID() string    { return "deepseek" }
func (d *deepSeek) Label() string { return d.label }
func (d *deepSeek) Kind() string  { return model.KindCredit }

type dsBalance struct {
	IsAvailable  bool `json:"is_available"`
	BalanceInfos []struct {
		Currency        string `json:"currency"`
		TotalBalance    string `json:"total_balance"`
		GrantedBalance  string `json:"granted_balance"`
		ToppedUpBalance string `json:"topped_up_balance"`
	} `json:"balance_infos"`
}

func (d *deepSeek) Fetch(ctx context.Context) (model.Status, error) {
	if d.apiKey == "" {
		return model.Status{}, fmt.Errorf("missing api_key")
	}
	var body dsBalance
	code, raw, err := getJSON(ctx, d.base+"/user/balance",
		map[string]string{"Authorization": "Bearer " + d.apiKey}, &body)
	if err != nil {
		return model.Status{}, err
	}
	if code != 200 {
		return model.Status{}, fmt.Errorf("deepseek %d: %s", code, snippet(raw, 160))
	}
	if len(body.BalanceInfos) == 0 {
		return model.Status{}, fmt.Errorf("deepseek returned no balance_infos")
	}

	// Prefer the USD ledger if the account has multiple currencies.
	idx := 0
	for i, bi := range body.BalanceInfos {
		if bi.Currency == "USD" {
			idx = i
			break
		}
	}
	bi := body.BalanceInfos[idx]

	st := model.Status{
		Provider: d.ID(),
		Label:    d.label,
		Kind:     d.Kind(),
		OK:       true,
		Unit:     bi.Currency,
		Balance:  fptr(parseFloat(bi.TotalBalance)),
		Extra: map[string]any{
			"is_available":      body.IsAvailable,
			"granted_balance":   parseFloat(bi.GrantedBalance),
			"topped_up_balance": parseFloat(bi.ToppedUpBalance),
		},
	}
	if !body.IsAvailable {
		st.Extra["note"] = "account marked unavailable"
	}
	return st, nil
}

func parseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}
