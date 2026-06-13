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

// anthropic reports Anthropic API billing via the Admin Cost Report endpoint.
// Anthropic exposes no prepaid-balance API, so this monitor has two modes:
//
//   - cost (default): month-to-date accrued spend.
//   - credit: when credit_balance is configured, reports remaining credit as
//     credit_balance − (spend since credit_as_of). Exact, since the cost report
//     gives precise spend; bump the baseline after buying more credits.
//
// Requires an Admin key (sk-ant-admin-...).
type anthropic struct {
	label     string
	adminKey  string
	base      string
	balance   float64   // configured baseline credit (0 => cost mode)
	asOf      time.Time // when the baseline was true
	hasCredit bool
}

func newAnthropic(m config.Monitor) *anthropic {
	base := m.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	a := &anthropic{label: m.Label, adminKey: m.AdminKey, base: base}
	if m.CreditBalance > 0 {
		a.hasCredit = true
		a.balance = m.CreditBalance
		a.asOf = parseAsOf(m.CreditAsOf)
	}
	return a
}

// parseAsOf accepts a date or RFC3339 timestamp; defaults to the start of the
// current UTC month so the baseline still produces a sensible number if omitted.
func parseAsOf(s string) time.Time {
	s = strings.TrimSpace(s)
	if s != "" {
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UTC()
			}
		}
	}
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// spendStart converts the credit baseline's as-of moment into the first UTC
// day whose spend should be subtracted. The cost report only has daily (UTC)
// buckets, so a baseline read mid-day must NOT include its own day: spend made
// earlier that day is already reflected in the console balance, and counting
// the full day again double-subtracts it (the "negative credits" bug). A
// date-only as_of (midnight) keeps the day inclusive.
func (a *anthropic) spendStart() time.Time {
	day := time.Date(a.asOf.Year(), a.asOf.Month(), a.asOf.Day(), 0, 0, 0, 0, time.UTC)
	if a.asOf.After(day) {
		return day.AddDate(0, 0, 1) // mid-day baseline: start with the next full day
	}
	return day
}

func (a *anthropic) ID() string    { return "anthropic" }
func (a *anthropic) Label() string { return a.label }
func (a *anthropic) Kind() string {
	if a.hasCredit {
		return model.KindCredit
	}
	return model.KindCost
}

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

	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC) // cost mode: MTD
	if a.hasCredit {
		start = a.spendStart()
	}

	var spend float64
	currency := "USD"
	if now.After(start) {
		var err error
		spend, currency, err = a.spendSince(ctx, start, now)
		if err != nil {
			return model.Status{}, err
		}
	}

	st := model.Status{
		Provider: a.ID(),
		Label:    a.label,
		Kind:     a.Kind(),
		OK:       true,
		Unit:     currency,
		Extra:    map[string]any{"since": start.Format("2006-01-02")},
	}

	if a.hasCredit {
		// Remaining prepaid credit = baseline − spend since the baseline date.
		remaining := a.balance - spend
		st.Balance = fptr(remaining)
		st.Total = fptr(a.balance)
		st.Used = fptr(spend)
		st.Window = "since " + start.Format("Jan 2")
		st.Extra["baseline"] = a.balance
		st.Extra["note"] = "balance = configured baseline − spend (no public balance API)"
		if remaining < 0 {
			// Prepaid credit can't really go below zero — a negative reading means
			// the baseline no longer matches reality (topped up, or set with a date
			// that double-counted same-day spend). Surface that.
			st.Extra["note"] = "negative balance: re-baseline credit_balance/credit_as_of " +
				"(use the current Console value with a full timestamp)"
		}
	} else {
		st.Used = fptr(spend)
		st.Window = "month-to-date"
	}
	return st, nil
}

// spendSince sums Admin cost-report amounts between start and now (following
// pagination), returning the total and the reported currency.
func (a *anthropic) spendSince(ctx context.Context, start, now time.Time) (float64, string, error) {
	headers := map[string]string{
		"x-api-key":         a.adminKey,
		"anthropic-version": "2023-06-01",
	}
	var total float64
	currency := "USD"
	page := ""

	for range 60 {
		q := url.Values{}
		q.Set("starting_at", start.Format(time.RFC3339))
		q.Set("ending_at", now.Format(time.RFC3339))
		if page != "" {
			q.Set("page", page)
		}
		endpoint := a.base + "/v1/organizations/cost_report?" + q.Encode()

		var rep costReport
		code, raw, err := getJSON(ctx, endpoint, headers, &rep)
		if err != nil {
			return 0, currency, err
		}
		if code == 401 || code == 403 {
			return 0, currency, fmt.Errorf("anthropic %d: admin key rejected (use sk-ant-admin-...)", code)
		}
		if code != 200 {
			return 0, currency, fmt.Errorf("anthropic %d: %s", code, snippet(raw, 160))
		}
		total += sumReportCents(rep, &currency)
		if !rep.HasMore || rep.NextPage == "" {
			break
		}
		page = rep.NextPage
	}
	// The Cost API reports amounts in USD lowest units (cents) — convert to USD.
	return total / 100, currency, nil
}

// sumReportCents totals one cost-report page's amounts (in cents) and tracks the
// reported currency.
func sumReportCents(rep costReport, currency *string) float64 {
	var cents float64
	for _, bucket := range rep.Data {
		for _, r := range bucket.Results {
			cents += float64(r.Amount)
			if r.Currency != "" {
				*currency = r.Currency
			}
		}
	}
	return cents
}
