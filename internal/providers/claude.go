package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/algonode/aimon/internal/config"
	"github.com/algonode/aimon/internal/model"
	"github.com/algonode/aimon/internal/oauth"
	"github.com/algonode/aimon/internal/store"
)

// Claude Code's public OAuth client. The manual flow redirects to an Anthropic
// page that prints `code#state` for the user to paste back.
const (
	claudeClientID    = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeAuthorize   = "https://claude.ai/oauth/authorize"
	claudeTokenURL    = "https://console.anthropic.com/v1/oauth/token"
	claudeRedirectURI = "https://console.anthropic.com/oauth/code/callback"
	// Best-effort usage endpoint; override per-monitor with `usage_url`.
	claudeDefaultUsageURL = "https://api.anthropic.com/api/oauth/usage"
)

// OAuthProvider is implemented by providers that authenticate interactively.
// The HTTP server uses it to drive the connect/disconnect flow.
type OAuthProvider interface {
	Provider
	OAuthConfig() oauth.Config
	// Manual reports whether this provider uses the paste-the-code flow
	// (vs a browser redirect back to aimon).
	Manual() bool
}

// claude reports Claude Pro/Max subscription usage via an OAuth access token.
type claude struct {
	label    string
	oauth    *oauth.Manager
	usageURL string
}

func newClaude(m config.Monitor, mgr *oauth.Manager) *claude {
	usage := m.UsageURL
	if usage == "" {
		usage = claudeDefaultUsageURL
	}
	return &claude{label: m.Label, oauth: mgr, usageURL: usage}
}

func (c *claude) ID() string    { return "claude" }
func (c *claude) Label() string { return c.label }
func (c *claude) Kind() string  { return model.KindSubscription }
func (c *claude) Manual() bool  { return true }

func (c *claude) OAuthConfig() oauth.Config {
	return oauth.Config{
		Provider:     c.ID(),
		AuthorizeURL: claudeAuthorize,
		TokenURL:     claudeTokenURL,
		ClientID:     claudeClientID,
		Scopes:       []string{"org:create_api_key", "user:profile", "user:inference"},
		RedirectURI:  claudeRedirectURI,
		FormEncoded:  false,
	}
}

func (c *claude) Fetch(ctx context.Context) (model.Status, error) {
	base := model.Status{Provider: c.ID(), Label: c.label, Kind: c.Kind()}

	token, err := c.oauth.AccessToken(ctx, c.label, c.OAuthConfig())
	if errors.Is(err, store.ErrNoToken) {
		base.NeedsAuth = true
		base.Error = "not connected — click Connect to authorize"
		return base, nil // a state, not a failure
	}
	if err != nil {
		base.NeedsAuth = true
		return base, fmt.Errorf("auth: %w", err)
	}

	// Best-effort usage lookup. The exact endpoint/shape for subscription usage
	// is not part of a stable public contract, so we parse defensively and fall
	// back to a plain "connected" status.
	var rawAny map[string]any
	code, _, ferr := getJSON(ctx, c.usageURL, map[string]string{
		"Authorization":     "Bearer " + token,
		"anthropic-version": "2023-06-01",
		"anthropic-beta":    "oauth-2025-04-20",
	}, &rawAny)

	base.OK = true
	base.Plan = "Pro/Max"
	base.Unit = "%"
	base.Extra = map[string]any{}

	if ferr != nil || code != 200 {
		base.Extra["usage"] = "unavailable"
		base.Extra["hint"] = "set `usage_url` for this monitor if your plan exposes a usage endpoint"
		if code != 0 {
			base.Extra["http_status"] = code
		}
		return base, nil
	}

	if util, reset, ok := parseClaudeUsage(rawAny); ok {
		base.Used = fptr(util * 100)
		base.Total = fptr(100)
		base.Balance = fptr((1 - util) * 100)
		if reset > 0 {
			base.ResetAt = &reset
		}
	}
	// Keep a compact copy of the raw response for debugging in the UI.
	if b, mErr := json.Marshal(rawAny); mErr == nil {
		base.Extra["raw"] = string(b)
	}
	return base, nil
}

// parseClaudeUsage hunts for a utilization fraction (0..1) and reset time in an
// unknown-shaped usage payload. Returns ok=false when nothing recognizable is
// found.
func parseClaudeUsage(m map[string]any) (util float64, resetAt int64, ok bool) {
	// Prefer the tightest (most-utilized) window if several are present.
	best := -1.0
	walk(m, func(key string, val any) {
		switch key {
		case "utilization", "used_pct", "usage_pct", "percent_used":
			if f, fok := toFloat(val); fok {
				if f > 1 { // expressed as a percentage
					f /= 100
				}
				if f > best {
					best = f
				}
			}
		case "resets_at", "reset_at", "resets":
			if t, tok := toUnix(val); tok && resetAt == 0 {
				resetAt = t
			}
		}
	})
	if best < 0 {
		return 0, resetAt, false
	}
	return best, resetAt, true
}

// walk visits every key/value pair in a nested map/array structure.
func walk(v any, fn func(key string, val any)) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			fn(k, val)
			walk(val, fn)
		}
	case []any:
		for _, val := range t {
			walk(val, fn)
		}
	}
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func toUnix(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case string:
		if t, err := time.Parse(time.RFC3339, n); err == nil {
			return t.Unix(), true
		}
	}
	return 0, false
}
