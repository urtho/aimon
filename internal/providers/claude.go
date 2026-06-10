package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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
	claudeAuthorize   = "https://claude.com/cai/oauth/authorize"
	claudeTokenURL    = "https://platform.claude.com/v1/oauth/token"
	claudeRedirectURI = "https://platform.claude.com/oauth/code/callback"
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
	authURL  string // authorize endpoint (override via config authorize_url)
}

func newClaude(m config.Monitor, mgr *oauth.Manager) *claude {
	usage := m.UsageURL
	if usage == "" {
		usage = claudeDefaultUsageURL
	}
	authURL := m.AuthorizeURL
	if authURL == "" {
		authURL = claudeAuthorize
	}
	return &claude{label: m.Label, oauth: mgr, usageURL: usage, authURL: authURL}
}

func (c *claude) ID() string    { return "claude" }
func (c *claude) Label() string { return c.label }
func (c *claude) Kind() string  { return model.KindSubscription }
func (c *claude) Manual() bool  { return true }

func (c *claude) OAuthConfig() oauth.Config {
	return oauth.Config{
		Provider:     c.ID(),
		AuthorizeURL: c.authURL,
		TokenURL:     claudeTokenURL,
		ClientID:     claudeClientID,
		// The live Claude Code binary requests this exact 6-scope union (order
		// preserved). A subset is rejected by the authorize endpoint with
		// "Invalid request format" even though the consent page renders.
		Scopes: []string{
			"org:create_api_key", "user:profile", "user:inference",
			"user:sessions:claude_code", "user:mcp_servers", "user:file_upload",
		},
		RedirectURI: claudeRedirectURI,
		FormEncoded: false, // token exchange is JSON (Content-Type: application/json)
		// `code=true` selects Anthropic's manual code-display flow; without it the
		// authorize page rejects with "Invalid request format".
		ExtraAuth: map[string]string{"code": "true"},
		// Emit params in the exact order the Claude Code CLI uses (the authorize
		// flow is order-sensitive in practice).
		ParamOrder: []string{
			"code", "client_id", "response_type", "redirect_uri",
			"scope", "code_challenge", "code_challenge_method", "state",
		},
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

	if util, reset, windows, ok := parseClaudeUsage(rawAny); ok {
		base.Used = fptr(util * 100)
		base.Total = fptr(100)
		base.Balance = fptr((1 - util) * 100)
		if reset > 0 {
			base.ResetAt = &reset
		}
		if len(windows) > 0 {
			base.Extra["windows"] = windows // per-window utilization %, for debugging
		}
	}
	// Keep a compact copy of the raw response for debugging in the UI.
	if b, mErr := json.Marshal(rawAny); mErr == nil {
		base.Extra["raw"] = string(b)
	}
	return base, nil
}

// parseClaudeUsage extracts rate-limit utilization from the OAuth usage
// endpoint. The response holds one object per limit window (five_hour,
// seven_day, seven_day_opus, ...), each with a `utilization` PERCENTAGE plus
// `resets_at`. The gauge shows the most-utilized window; windows maps each
// window name to its raw percentage.
//
// utilization is always a percent (0-100). Do NOT re-scale values ≤ 1: a
// freshly reset window reporting e.g. 0.8 means 0.8%, and interpreting it as a
// 0..1 fraction made the gauge spike to ~100% whenever any window dipped
// below 1% — the "alternating 100% used" bug.
func parseClaudeUsage(m map[string]any) (frac float64, resetAt int64, windows map[string]any, ok bool) {
	windows = map[string]any{}
	best := -1.0
	var bestReset int64

	var scan func(name string, v any)
	scan = func(name string, v any) {
		switch t := v.(type) {
		case map[string]any:
			if u, uok := toFloat(t["utilization"]); uok {
				if name != "" {
					windows[name] = u
				}
				f := u / 100
				if f < 0 {
					f = 0
				} else if f > 1 {
					f = 1
				}
				if f > best {
					best = f
					bestReset = 0
					if r, rok := toUnix(t["resets_at"]); rok {
						bestReset = r
					}
				}
				return
			}
			for k, child := range t {
				childName := k
				if name != "" {
					childName = name + "." + k
				}
				scan(childName, child)
			}
		case []any:
			for i, child := range t {
				scan(fmt.Sprintf("%s[%d]", name, i), child)
			}
		}
	}
	scan("", m)

	if best < 0 {
		return 0, 0, windows, false
	}
	return best, bestReset, windows, true
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
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
