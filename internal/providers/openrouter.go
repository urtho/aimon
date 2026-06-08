package providers

import (
	"context"
	"fmt"

	"github.com/algonode/aimon/internal/config"
	"github.com/algonode/aimon/internal/model"
)

// openRouter reports prepaid credit from https://openrouter.ai/api/v1/credits,
// enriched with rate-limit info from /api/v1/key when available.
type openRouter struct {
	label  string
	apiKey string
	base   string
}

func newOpenRouter(m config.Monitor) *openRouter {
	base := m.BaseURL
	if base == "" {
		base = "https://openrouter.ai/api/v1"
	}
	return &openRouter{label: m.Label, apiKey: m.APIKey, base: base}
}

func (o *openRouter) ID() string    { return "openrouter" }
func (o *openRouter) Label() string { return o.label }
func (o *openRouter) Kind() string  { return model.KindCredit }

type orCredits struct {
	Data struct {
		TotalCredits float64 `json:"total_credits"`
		TotalUsage   float64 `json:"total_usage"`
	} `json:"data"`
}

type orKey struct {
	Data struct {
		Label       string   `json:"label"`
		Usage       float64  `json:"usage"`
		Limit       *float64 `json:"limit"`
		LimitRemain *float64 `json:"limit_remaining"`
		IsFreeTier  bool     `json:"is_free_tier"`
		RateLimit   struct {
			Requests float64 `json:"requests"`
			Interval string  `json:"interval"`
		} `json:"rate_limit"`
	} `json:"data"`
}

func (o *openRouter) Fetch(ctx context.Context) (model.Status, error) {
	if o.apiKey == "" {
		return model.Status{}, fmt.Errorf("missing api_key")
	}
	auth := map[string]string{"Authorization": "Bearer " + o.apiKey}

	var cr orCredits
	code, raw, err := getJSON(ctx, o.base+"/credits", auth, &cr)
	if err != nil {
		return model.Status{}, err
	}
	if code != 200 {
		return model.Status{}, fmt.Errorf("openrouter %d: %s", code, snippet(raw, 160))
	}

	remaining := cr.Data.TotalCredits - cr.Data.TotalUsage
	st := model.Status{
		Provider: o.ID(),
		Label:    o.label,
		Kind:     o.Kind(),
		OK:       true,
		Unit:     "USD",
		Balance:  fptr(remaining),
		Total:    fptr(cr.Data.TotalCredits),
		Used:     fptr(cr.Data.TotalUsage),
		Extra:    map[string]any{},
	}

	// Best-effort enrichment; never fail the whole fetch on this.
	var key orKey
	if c, _, kerr := getJSON(ctx, o.base+"/key", auth, &key); kerr == nil && c == 200 {
		st.Extra["is_free_tier"] = key.Data.IsFreeTier
		if key.Data.RateLimit.Requests > 0 {
			st.Extra["rate_limit"] = fmt.Sprintf("%g/%s", key.Data.RateLimit.Requests, key.Data.RateLimit.Interval)
		}
		if key.Data.Limit != nil {
			st.Extra["key_limit"] = *key.Data.Limit
		}
	}
	return st, nil
}
