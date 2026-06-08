package providers

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/algonode/aimon/internal/config"
	"github.com/algonode/aimon/internal/model"
)

// anthropic reports month-to-date API spend via the Admin Cost Report endpoint.
// Anthropic exposes no prepaid-balance API, so this monitor tracks accrued cost
// rather than a remaining balance. Requires an Admin key (sk-ant-admin-...).
type anthropic struct {
	label    string
	adminKey string
	base     string
}

func newAnthropic(m config.Monitor) *anthropic {
	base := m.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	return &anthropic{label: m.Label, adminKey: m.AdminKey, base: base}
}

func (a *anthropic) ID() string    { return "anthropic" }
func (a *anthropic) Label() string { return a.label }
func (a *anthropic) Kind() string  { return model.KindCost }

// flexFloat decodes a JSON number OR a numeric string (the cost report returns
// amounts as strings like "0.42").
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*f = flexFloat(v)
	return nil
}

type costReport struct {
	Data []struct {
		Results []struct {
			Currency string    `json:"currency"`
			Amount   flexFloat `json:"amount"`
		} `json:"results"`
	} `json:"data"`
	HasMore  bool   `json:"has_more"`
	NextPage string `json:"next_page"`
}

func (a *anthropic) Fetch(ctx context.Context) (model.Status, error) {
	if a.adminKey == "" {
		return model.Status{}, fmt.Errorf("missing admin_key (needs sk-ant-admin-...)")
	}
	headers := map[string]string{
		"x-api-key":         a.adminKey,
		"anthropic-version": "2023-06-01",
	}

	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	var total float64
	currency := "USD"
	page := ""

	// Follow pagination (default bucket_width=1d yields one bucket per day).
	for i := 0; i < 40; i++ {
		q := url.Values{}
		q.Set("starting_at", monthStart.Format(time.RFC3339))
		q.Set("ending_at", now.Format(time.RFC3339))
		if page != "" {
			q.Set("page", page)
		}
		endpoint := a.base + "/v1/organizations/cost_report?" + q.Encode()

		var rep costReport
		code, raw, err := getJSON(ctx, endpoint, headers, &rep)
		if err != nil {
			return model.Status{}, err
		}
		if code == 401 || code == 403 {
			return model.Status{}, fmt.Errorf("anthropic %d: admin key rejected (use sk-ant-admin-...)", code)
		}
		if code != 200 {
			return model.Status{}, fmt.Errorf("anthropic %d: %s", code, snippet(raw, 160))
		}
		for _, bucket := range rep.Data {
			for _, r := range bucket.Results {
				total += float64(r.Amount)
				if r.Currency != "" {
					currency = r.Currency
				}
			}
		}
		if !rep.HasMore || rep.NextPage == "" {
			break
		}
		page = rep.NextPage
	}

	return model.Status{
		Provider: a.ID(),
		Label:    a.label,
		Kind:     a.Kind(),
		OK:       true,
		Unit:     currency,
		Used:     fptr(total),
		Window:   "month-to-date",
		Extra: map[string]any{
			"since": monthStart.Format("2006-01-02"),
		},
	}, nil
}
