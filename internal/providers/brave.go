package providers

import (
	"context"
	"fmt"

	"github.com/algonode/aimon/internal/config"
	"github.com/algonode/aimon/internal/model"
)

// braveDefaultPrice is Brave Search API's price per request in USD.
const braveDefaultPrice = 0.005

// brave estimates what is left of a Brave Search API balance. Brave has no
// balance API, so: credit_balance − billed requests since then × credit_price.
// base_url is a counter file whose size is the number of billed requests (see
// algoup deploy/searxng-firecrawl); credit_base_count is its size when
// credit_balance was read. Bump both when the balance changes.
type brave struct {
	label     string
	url       string
	baseline  float64
	asOf      string
	baseCount int64
	price     float64
}

func newBrave(m config.Monitor) *brave {
	price := m.CreditPrice
	if price <= 0 {
		price = braveDefaultPrice
	}
	return &brave{label: m.Label, url: m.BaseURL, baseline: m.CreditBalance, asOf: m.CreditAsOf,
		baseCount: m.CreditBaseCount, price: price}
}

func (b *brave) ID() string    { return "brave" }
func (b *brave) Label() string { return b.label }
func (b *brave) Kind() string  { return model.KindCredit }

func (b *brave) Fetch(ctx context.Context) (model.Status, error) {
	if b.url == "" || b.baseline <= 0 {
		return model.Status{}, fmt.Errorf("base_url and credit_balance are required")
	}
	code, raw, err := getJSON(ctx, b.url, nil, nil)
	if err != nil {
		return model.Status{}, err
	}
	if code != 200 {
		return model.Status{}, fmt.Errorf("brave counter %d: %s", code, snippet(raw, 160))
	}
	requests := int64(len(raw)) - b.baseCount
	if requests < 0 {
		return model.Status{}, fmt.Errorf("brave counter is below credit_base_count")
	}
	used := float64(requests) * b.price
	return model.Status{
		Provider: b.ID(),
		Label:    b.label,
		Kind:     b.Kind(),
		OK:       true,
		Unit:     "USD",
		Balance:  fptr(b.baseline - used),
		Total:    fptr(b.baseline),
		Used:     fptr(used),
		Window:   "since " + b.asOf,
		Extra: map[string]any{
			"requests": requests,
			"note":     fmt.Sprintf("estimate: $%g on %s minus %d requests at $%g", b.baseline, b.asOf, requests, b.price),
		},
	}, nil
}
