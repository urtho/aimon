// Package model holds the normalized domain types shared across the app:
// the per-provider Status that the UI renders and the DB persists.
package model

// Kind classifies what a monitor reports.
const (
	KindCredit       = "credit"       // prepaid balance that draws down (DeepSeek, OpenRouter)
	KindCost         = "cost"         // accrued spend over a window (Anthropic Admin cost report)
	KindSubscription = "subscription" // plan usage against rate limits (Claude, x.ai)
)

// Status is the normalized snapshot of a single monitored account at a point
// in time. Pointers are used for numeric fields so "absent" is distinct from 0.
type Status struct {
	Provider string `json:"provider"` // stable type id, e.g. "deepseek"
	Label    string `json:"label"`    // human label / account name, e.g. "DeepSeek"
	Kind     string `json:"kind"`     // one of the Kind* constants

	OK    bool   `json:"ok"`              // did the last fetch succeed?
	Error string `json:"error,omitempty"` // populated when OK is false

	// Money / quota figures. Semantics depend on Kind:
	//   credit  -> Balance = remaining, Total = purchased, Used = consumed
	//   cost    -> Used = spend in window, Total/Balance usually nil
	//   subscription -> Used/Total/Balance expressed in Unit (e.g. requests, %)
	Balance *float64 `json:"balance,omitempty"`
	Total   *float64 `json:"total,omitempty"`
	Used    *float64 `json:"used,omitempty"`
	Unit    string   `json:"unit,omitempty"` // "USD", "CNY", "requests", "%"...

	Plan    string `json:"plan,omitempty"`     // subscription plan name
	Window  string `json:"window,omitempty"`   // human window for cost/usage, e.g. "month-to-date"
	ResetAt *int64 `json:"reset_at,omitempty"` // unix seconds when a quota window resets

	// NeedsAuth flags that an interactive (OAuth) login is required before this
	// monitor can report. The UI surfaces a "Connect" affordance for these.
	NeedsAuth bool `json:"needs_auth,omitempty"`

	// Extra carries provider-specific fields that don't fit the common shape.
	Extra map[string]any `json:"extra,omitempty"`

	UpdatedAt int64 `json:"updated_at"` // unix seconds of this snapshot
}

// ID is the stable identity of a monitor: provider + label. Used as the map key
// for "latest status" lookups and SSE de-duplication.
func (s Status) ID() string { return s.Provider + "/" + s.Label }

// Remaining returns the fraction (0..1) of quota/credit left when it can be
// derived, plus ok=false when it can't. Used to drive the gauge fill.
func (s Status) Remaining() (float64, bool) {
	switch {
	case s.Balance != nil && s.Total != nil && *s.Total > 0:
		return clamp01(*s.Balance / *s.Total), true
	case s.Used != nil && s.Total != nil && *s.Total > 0:
		return clamp01(1 - *s.Used/(*s.Total)), true
	default:
		return 0, false
	}
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}
