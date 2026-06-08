package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/algonode/aimon/internal/config"
	"github.com/algonode/aimon/internal/model"
	"github.com/algonode/aimon/internal/oauth"
	"github.com/algonode/aimon/internal/store"
)

// grokBillingURL is the Connect/gRPC-web endpoint the Grok CLI's `/usage`
// command reads. It works with a Connect-JSON POST and a SuperGrok OAuth bearer.
const grokBillingURL = "https://grok.com/grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig"

// x.ai's public desktop OAuth client (the same one the Grok CLI / Hermes use —
// public metadata, not a secret). The flow is loopback-redirect + PKCE; aimon
// uses the manual paste-the-code variant so it works for remote/headless hosts.
const (
	xaiClientID    = "b1a00492-073a-47ea-816f-4c329264a828"
	xaiAuthorize   = "https://auth.x.ai/oauth2/authorize"
	xaiTokenURL    = "https://auth.x.ai/oauth2/token"
	xaiRedirectURI = "http://127.0.0.1:56121/callback"
	xaiScope       = "openid profile email offline_access grok-cli:access api:access"
)

// xai monitors x.ai / SuperGrok. SuperGrok usage is exposed through the same
// billing RPC that the Grok CLI's `/usage` command uses; it needs the
// subscription OAuth bearer token. That token is provided one of three ways:
//
//   - token_file:   path to a JSON file an external tool keeps refreshed
//     (e.g. ~/.hermes/auth.json or ~/.grok/auth.json). Read live
//     each poll so it always reflects the current access token.
//   - bearer_token: a static token pasted into the config.
//   - api_key:      fallback that only validates the API key (no SuperGrok usage).
type xai struct {
	label     string
	apiKey    string
	bearer    string
	tokenFile string
	usageURL  string
	base      string
	oauth     *oauth.Manager
}

func newXAI(m config.Monitor, mgr *oauth.Manager) *xai {
	base := m.BaseURL
	if base == "" {
		base = "https://api.x.ai/v1"
	}
	usage := m.UsageURL
	if usage == "" {
		usage = grokBillingURL
	}
	return &xai{
		label:     m.Label,
		apiKey:    m.APIKey,
		bearer:    m.BearerToken,
		tokenFile: m.TokenFile,
		usageURL:  usage,
		base:      base,
		oauth:     mgr,
	}
}

func (x *xai) ID() string    { return "xai" }
func (x *xai) Label() string { return x.label }
func (x *xai) Kind() string  { return model.KindSubscription }
func (x *xai) Manual() bool  { return true }

// OAuthConfig returns the SuperGrok OAuth (PKCE) configuration so x.ai gets the
// same in-app Connect flow as Claude.
func (x *xai) OAuthConfig() oauth.Config {
	return oauth.Config{
		Provider:            x.ID(),
		AuthorizeURL:        xaiAuthorize,
		TokenURL:            xaiTokenURL,
		ClientID:            xaiClientID,
		Scopes:              strings.Fields(xaiScope),
		RedirectURI:         xaiRedirectURI,
		FormEncoded:         true,
		Nonce:               true,
		ChallengeOnExchange: true,
		ExtraAuth:           map[string]string{"plan": "generic", "referrer": "hermes-agent"},
	}
}

func (x *xai) Fetch(ctx context.Context) (model.Status, error) {
	// Token priority: live file > static bearer > in-app OAuth token.
	token := x.bearer
	if x.tokenFile != "" {
		t, err := readTokenFile(x.tokenFile)
		if err != nil {
			return model.Status{}, fmt.Errorf("read token_file: %w", err)
		}
		token = t
	}
	if token == "" && x.oauth != nil {
		t, err := x.oauth.AccessToken(ctx, x.label, x.OAuthConfig())
		switch {
		case err == nil:
			token = t
		case errors.Is(err, store.ErrNoToken):
			// fall through — try api_key or prompt to connect
		default:
			// Connected previously but the token can't be refreshed.
			st := model.Status{Provider: x.ID(), Label: x.label, Kind: x.Kind(), NeedsAuth: true}
			st.Error = "SuperGrok auth expired — reconnect"
			return st, fmt.Errorf("xai auth: %w", err)
		}
	}

	if token != "" {
		return x.fetchSuperGrok(ctx, token)
	}
	if x.apiKey != "" {
		return x.fetchAPIKey(ctx)
	}
	st := model.Status{Provider: x.ID(), Label: x.label, Kind: x.Kind(), NeedsAuth: true}
	st.Error = "not connected — click Connect to authorize SuperGrok"
	return st, nil
}

// grokVal mirrors the proto int wrapper {"val": N} used in the billing response.
type grokVal struct {
	Val flexFloat `json:"val"`
}

type grokCredits struct {
	BillingCycle struct {
		BillingPeriodStart string `json:"billingPeriodStart"`
		BillingPeriodEnd   string `json:"billingPeriodEnd"`
	} `json:"billingCycle"`
	MonthlyLimit grokVal `json:"monthlyLimit"`
	Usage        struct {
		IncludedUsed grokVal `json:"includedUsed"`
		OnDemandUsed grokVal `json:"onDemandUsed"`
		TotalUsed    grokVal `json:"totalUsed"`
	} `json:"usage"`
	OnDemandEnabled bool `json:"onDemandEnabled"`
	// Connect errors come back as {"code","message"}.
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (x *xai) fetchSuperGrok(ctx context.Context, token string) (model.Status, error) {
	headers := map[string]string{
		"Authorization":            "Bearer " + token,
		"Connect-Protocol-Version": "1",
		"Origin":                   "https://grok.com",
	}
	var cr grokCredits
	code, raw, err := postJSON(ctx, x.usageURL, headers, map[string]any{}, &cr)
	if err != nil {
		return model.Status{}, err
	}
	if code != 200 {
		msg := cr.Message
		if msg == "" {
			msg = snippet(raw, 160)
		}
		return model.Status{}, fmt.Errorf("grok billing %d: %s", code, msg)
	}

	limit := float64(cr.MonthlyLimit.Val)
	used := float64(cr.Usage.TotalUsed.Val)

	st := model.Status{
		Provider: x.ID(),
		Label:    x.label,
		Kind:     x.Kind(),
		OK:       true,
		Plan:     "SuperGrok",
		Window:   "billing cycle",
		Extra: map[string]any{
			"included_used":     float64(cr.Usage.IncludedUsed.Val),
			"on_demand_used":    float64(cr.Usage.OnDemandUsed.Val),
			"on_demand_enabled": cr.OnDemandEnabled,
			"raw":               string(raw),
		},
	}

	if limit > 0 {
		// Values are integer credits/cents; the ratio is unit-independent.
		pct := used / limit * 100
		st.Unit = "%"
		st.Used = fptr(pct)
		st.Total = fptr(100)
		st.Balance = fptr(100 - pct)
		st.Extra["monthly_limit"] = limit
		st.Extra["total_used"] = used
	} else {
		// No limit reported (e.g. unlimited tier): show raw usage instead.
		st.Used = fptr(used)
	}

	if t, err := time.Parse(time.RFC3339, cr.BillingCycle.BillingPeriodEnd); err == nil {
		u := t.Unix()
		st.ResetAt = &u
	}
	return st, nil
}

func (x *xai) fetchAPIKey(ctx context.Context) (model.Status, error) {
	var key struct {
		Name        string   `json:"name"`
		RedactedKey string   `json:"redacted_api_key"`
		ACLs        []string `json:"acls"`
		Blocked     bool     `json:"api_key_blocked"`
		Disabled    bool     `json:"api_key_disabled"`
		TeamBlocked bool     `json:"team_blocked"`
	}
	code, raw, err := getJSON(ctx, x.base+"/api-key",
		map[string]string{"Authorization": "Bearer " + x.apiKey}, &key)
	if err != nil {
		return model.Status{}, err
	}
	if code != 200 {
		return model.Status{}, fmt.Errorf("xai %d: %s", code, snippet(raw, 160))
	}

	active := !key.Blocked && !key.Disabled && !key.TeamBlocked
	st := model.Status{
		Provider: x.ID(),
		Label:    x.label,
		Kind:     x.Kind(),
		OK:       active,
		Plan:     "API key",
		Extra: map[string]any{
			"key_name": key.Name,
			"key":      key.RedactedKey,
			"acls":     len(key.ACLs),
			"active":   active,
			"note":     "API-key status only; set token_file/bearer_token for SuperGrok usage",
		},
	}
	if !active {
		st.Error = "x.ai API key is blocked or disabled"
	}
	return st, nil
}

// readTokenFile extracts a bearer token from a JSON credential file written by
// tools like Hermes Agent (~/.hermes/auth.json) or the Grok CLI (~/.grok/auth.json).
func readTokenFile(path string) (string, error) {
	path = expandHome(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return "", err
	}
	// Grok CLI stores the token under "key"; OAuth tools use "access_token".
	for _, k := range []string{"access_token", "key", "token", "bearer"} {
		if v, ok := m[k].(string); ok && v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("no token field (access_token/key) in %s", path)
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}
