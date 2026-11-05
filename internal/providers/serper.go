package providers

import (
	"context"
	"fmt"

	"github.com/algonode/aimon/internal/config"
	"github.com/algonode/aimon/internal/model"
)

// serperDefaultPrice is the Starter pack's price per credit in USD ($50 for 50k).
const serperDefaultPrice = 0.001

// serper reports prepaid credit from https://google.serper.dev/account. The API
// counts credits; Balance is in USD at credit_price so it reads like the other
// prepaid accounts, and the raw count stays in Extra.
type serper struct {
	label  string
	apiKey string
	base   string
	price  float64
}

func newSerper(m config.Monitor) *serper {
	base := m.BaseURL
	if base == "" {
		base = "https://google.serper.dev"
	}
	price := m.CreditPrice
	if price <= 0 {
		price = serperDefaultPrice
	}
	return &serper{label: m.Label, apiKey: m.APIKey, base: base, price: price}
}

func (s *serper) ID() string    { return "serper" }
func (s *serper) Label() string { return s.label }
func (s *serper) Kind() string  { return model.KindCredit }

func (s *serper) Fetch(ctx context.Context) (model.Status, error) {
	if s.apiKey == "" {
		return model.Status{}, fmt.Errorf("missing api_key")
	}
	var body struct {
		Balance *float64 `json:"balance"`
	}
	code, raw, err := getJSON(ctx, s.base+"/account", map[string]string{"X-API-KEY": s.apiKey}, &body)
	if code == 0 {
		return model.Status{}, err
	}
	if code != 200 {
		// Checked first: an error page need not be JSON, and its status says more.
		return model.Status{}, fmt.Errorf("serper %d: %s", code, snippet(raw, 160))
	}
	if err != nil {
		return model.Status{}, err
	}
	if body.Balance == nil {
		return model.Status{}, fmt.Errorf("serper returned no balance")
	}
	return model.Status{
		Provider: s.ID(),
		Label:    s.label,
		Kind:     s.Kind(),
		OK:       true,
		Unit:     "USD",
		Balance:  fptr(*body.Balance * s.price),
		Extra: map[string]any{
			"credits": *body.Balance,
			"note":    fmt.Sprintf("%.0f credits at $%g each", *body.Balance, s.price),
		},
	}, nil
}
