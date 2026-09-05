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

// grokCreditsURL is the REST endpoint Grok Build's `/usage` command reads
// (`GET /v1/billing?format=credits`). It returns the SuperGrok / X Premium+
// shared weekly usage pool as a percentage.
const grokCreditsURL = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"

// x.ai's public desktop OAuth client (the same one Grok Build uses —
// public metadata, not a secret). The flow is loopback-redirect + PKCE; aimon
// uses the manual paste-the-code variant so it works for remote/headless hosts.
const (
	xaiClientID    = "b1a00492-073a-47ea-816f-4c329264a828"
	xaiAuthorize   = "https://auth.x.ai/oauth2/authorize"
	xaiTokenURL    = "https://auth.x.ai/oauth2/token"
	xaiRedirectURI = "http://127.0.0.1:56121/callback"
	xaiScope       = "openid profile email offline_access grok-cli:access api:access"
)

// xai monitors Grok subscription usage (the shared weekly compute pool).
// That meter is exposed through the same billing endpoint Grok Build's
// `/usage` command uses, and needs the consumer OAuth bearer token. The
// token is provided one of three ways:
//
//   - token_file:   path to a JSON file an external tool keeps refreshed
//     (e.g. ~/.grok/auth.json). Read live each poll so it always
//     reflects the current access token.
//   - bearer_token: a static token pasted into the config.
//   - api_key:      fallback that only validates the API key (no Grok usage).
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
		usage = grokCreditsURL
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

// OAuthConfig returns the Grok OAuth (PKCE) configuration so x.ai gets the
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
		ExtraAuth:           map[string]string{"plan": "generic", "referrer": "grok-cli"},
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
			st.Error = "Grok auth expired — reconnect"
			return st, fmt.Errorf("xai auth: %w", err)
		}
	}

	if token != "" {
		return x.fetchGrokUsage(ctx, token)
	}
	if x.apiKey != "" {
		return x.fetchAPIKey(ctx)
	}
	st := model.Status{Provider: x.ID(), Label: x.label, Kind: x.Kind(), NeedsAuth: true}
	st.Error = "not connected — click Connect to authorize Grok"
	return st, nil
}

// grokVal mirrors the proto int wrapper {"val": N} used in the billing response.
type grokVal struct {
	Val flexFloat `json:"val"`
}

type grokPeriod struct {
	Type  string `json:"type"`
	Start string `json:"start"`
	End   string `json:"end"`
}

type grokProductUsage struct {
	Product      string  `json:"product"`
	UsagePercent float64 `json:"usagePercent"`
}

// grokCreditsConfig is the inner object of GET /v1/billing?format=credits
// (Grok Build `/usage`) and of GetGrokCreditsConfig.
type grokCreditsConfig struct {
	CreditUsagePercent *float64           `json:"creditUsagePercent"`
	CurrentPeriod      *grokPeriod        `json:"currentPeriod"`
	MonthlyLimit       grokVal            `json:"monthlyLimit"`
	Used               grokVal            `json:"used"`
	OnDemandCap        grokVal            `json:"onDemandCap"`
	OnDemandUsed       grokVal            `json:"onDemandUsed"`
	PrepaidBalance     grokVal            `json:"prepaidBalance"`
	IsUnified          *bool              `json:"isUnifiedBillingUser"`
	BillingPeriodStart string             `json:"billingPeriodStart"`
	BillingPeriodEnd   string             `json:"billingPeriodEnd"`
	ProductUsage       []grokProductUsage `json:"productUsage"`
	SubscriptionTier   string             `json:"subscriptionTier"`
}

type grokCreditsEnvelope struct {
	Config           *grokCreditsConfig `json:"config"`
	OnDemandEnabled  *bool              `json:"onDemandEnabled"`
	SubscriptionTier string             `json:"subscription_tier"`
	BillingCycle     struct {
		BillingPeriodStart string `json:"billingPeriodStart"`
		BillingPeriodEnd   string `json:"billingPeriodEnd"`
	} `json:"billingCycle"`
	MonthlyLimit grokVal `json:"monthlyLimit"`
	Usage        struct {
		IncludedUsed grokVal `json:"includedUsed"`
		OnDemandUsed grokVal `json:"onDemandUsed"`
		TotalUsed    grokVal `json:"totalUsed"`
	} `json:"usage"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (x *xai) fetchGrokUsage(ctx context.Context, token string) (model.Status, error) {
	headers := map[string]string{
		"Authorization":         "Bearer " + token,
		"Accept":                "application/json",
		"X-XAI-Token-Auth":      "xai-grok-cli",
		"x-grok-client-version": "aimon",
	}

	var (
		code int
		raw  []byte
		err  error
	)
	if isConnectRPC(x.usageURL) {
		headers["Connect-Protocol-Version"] = "1"
		headers["Origin"] = "https://grok.com"
		headers["Content-Type"] = "application/json"
		code, raw, err = postJSON(ctx, x.usageURL, headers, map[string]any{}, nil)
	} else {
		code, raw, err = getJSON(ctx, x.usageURL, headers, nil)
	}
	if err != nil {
		return model.Status{}, err
	}
	if code != 200 {
		return model.Status{}, fmt.Errorf("grok billing %d: %s", code, snippet(raw, 160))
	}
	if len(bytesTrimSpace(raw)) == 0 {
		return model.Status{}, fmt.Errorf("grok billing: empty response")
	}

	st, err := parseGrokBilling(x.label, raw)
	if err != nil {
		return model.Status{}, err
	}
	return st, nil
}

func isConnectRPC(u string) bool {
	return strings.Contains(u, "GetGrokCreditsConfig") ||
		strings.Contains(u, "GetGrokBuildBillingConfig")
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

// parseGrokBilling turns a Grok billing JSON body into a subscription Status.
// It understands the weekly credits-config shape (`config.creditUsagePercent`)
// that `/usage` uses, and the older monthlyLimit/usage cents shape.
func parseGrokBilling(label string, raw []byte) (model.Status, error) {
	var env grokCreditsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return model.Status{}, fmt.Errorf("decode grok billing: %w", err)
	}
	if env.Code != "" && env.Message != "" {
		return model.Status{}, fmt.Errorf("grok billing: %s", env.Message)
	}

	st := model.Status{
		Provider: "xai",
		Label:    label,
		Kind:     model.KindSubscription,
		OK:       true,
		Plan:     "Grok",
		Unit:     "%",
		Extra:    map[string]any{},
	}
	if env.SubscriptionTier != "" {
		st.Plan = env.SubscriptionTier
	}

	if env.Config != nil {
		return statusFromCreditsConfig(st, env)
	}
	return statusFromLegacyBilling(st, env)
}

func statusFromCreditsConfig(st model.Status, env grokCreditsEnvelope) (model.Status, error) {
	cfg := env.Config
	if cfg.SubscriptionTier != "" {
		st.Plan = cfg.SubscriptionTier
	}

	pct := 0.0
	havePct := cfg.CreditUsagePercent != nil
	if havePct {
		pct = *cfg.CreditUsagePercent
	}
	// proto3 omits 0.0. A current period still means "0% used this window".
	// A config with only the deprecated monthlyLimit/used cents fields falls
	// through to the legacy meter.
	if !havePct && cfg.CurrentPeriod == nil && float64(cfg.MonthlyLimit.Val) > 0 {
		legacy := grokCreditsEnvelope{MonthlyLimit: cfg.MonthlyLimit, SubscriptionTier: env.SubscriptionTier}
		legacy.Usage.TotalUsed = cfg.Used
		legacy.Usage.OnDemandUsed = cfg.OnDemandUsed
		legacy.BillingCycle.BillingPeriodStart = cfg.BillingPeriodStart
		legacy.BillingCycle.BillingPeriodEnd = cfg.BillingPeriodEnd
		return statusFromLegacyBilling(st, legacy)
	}
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}

	periodType := ""
	end := cfg.BillingPeriodEnd
	start := cfg.BillingPeriodStart
	if cfg.CurrentPeriod != nil {
		periodType = cfg.CurrentPeriod.Type
		if cfg.CurrentPeriod.End != "" {
			end = cfg.CurrentPeriod.End
		}
		if cfg.CurrentPeriod.Start != "" {
			start = cfg.CurrentPeriod.Start
		}
	}
	spanName, window := grokWindow(periodType)
	st.Window = window
	st.Used = fptr(pct)
	st.Total = fptr(100)
	st.Balance = fptr(100 - pct)

	var resetUnix int64
	if t, ok := parseGrokTime(end); ok {
		resetUnix = t.Unix()
		st.ResetAt = &resetUnix
	}
	st.Spans = []model.Span{{Name: spanName, Used: pct, ResetAt: resetUnix}}

	st.Extra["prepaid_cents"] = float64(cfg.PrepaidBalance.Val)
	st.Extra["on_demand_used"] = float64(cfg.OnDemandUsed.Val)
	st.Extra["on_demand_cap"] = float64(cfg.OnDemandCap.Val)
	if start != "" {
		st.Extra["period_start"] = start
	}
	if end != "" {
		st.Extra["period_end"] = end
	}
	if env.OnDemandEnabled != nil {
		st.Extra["on_demand_enabled"] = *env.OnDemandEnabled
	}
	if len(cfg.ProductUsage) > 0 {
		products := make(map[string]float64, len(cfg.ProductUsage))
		for _, p := range cfg.ProductUsage {
			name := strings.TrimPrefix(p.Product, "PRODUCT_")
			name = strings.TrimPrefix(name, "Grok")
			if name == "" {
				name = p.Product
			}
			products[name] = p.UsagePercent
		}
		st.Extra["products"] = products
	}
	return st, nil
}

func statusFromLegacyBilling(st model.Status, env grokCreditsEnvelope) (model.Status, error) {
	limit := float64(env.MonthlyLimit.Val)
	used := float64(env.Usage.TotalUsed.Val)
	st.Window = "billing cycle"
	st.Extra["included_used"] = float64(env.Usage.IncludedUsed.Val)
	st.Extra["on_demand_used"] = float64(env.Usage.OnDemandUsed.Val)
	if env.OnDemandEnabled != nil {
		st.Extra["on_demand_enabled"] = *env.OnDemandEnabled
	}

	end := env.BillingCycle.BillingPeriodEnd
	var resetUnix int64
	if t, ok := parseGrokTime(end); ok {
		resetUnix = t.Unix()
		st.ResetAt = &resetUnix
	}

	if limit > 0 {
		pct := used / limit * 100
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		st.Used = fptr(pct)
		st.Total = fptr(100)
		st.Balance = fptr(100 - pct)
		st.Extra["monthly_limit"] = limit
		st.Extra["total_used"] = used
		st.Spans = []model.Span{{Name: "1mo", Used: pct, ResetAt: resetUnix}}
		return st, nil
	}
	// No limit and no weekly percent: connected but no meter.
	st.Used = fptr(used)
	st.Unit = ""
	return st, nil
}

func grokWindow(periodType string) (span, window string) {
	u := strings.ToUpper(periodType)
	switch {
	case strings.Contains(u, "WEEK"):
		return "1w", "weekly"
	case strings.Contains(u, "MONTH"):
		return "1mo", "monthly"
	default:
		return "1w", "weekly"
	}
}

func parseGrokTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
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
			"note":     "API-key status only; set oauth/token_file for Grok subscription usage",
		},
	}
	if !active {
		st.Error = "x.ai API key is blocked or disabled"
	}
	return st, nil
}

// readTokenFile extracts a bearer token from a JSON credential file written by
// the Grok CLI (~/.grok/auth.json, nested under the issuer key) or similar
// OAuth tools (flat access_token / key).
func readTokenFile(path string) (string, error) {
	path = expandHome(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return "", err
	}
	if t := findToken(v); t != "" {
		return t, nil
	}
	return "", fmt.Errorf("no token field (access_token/key) in %s", path)
}

func findToken(v any) string {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range []string{"access_token", "key", "token", "bearer"} {
			if s, ok := t[k].(string); ok && s != "" {
				return s
			}
		}
		for _, child := range t {
			if s := findToken(child); s != "" {
				return s
			}
		}
	case []any:
		for _, child := range t {
			if s := findToken(child); s != "" {
				return s
			}
		}
	}
	return ""
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}
