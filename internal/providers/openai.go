package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/algonode/aimon/internal/config"
	"github.com/algonode/aimon/internal/model"
	"github.com/algonode/aimon/internal/oauth"
	"github.com/algonode/aimon/internal/store"
)

// Codex's public OAuth client and subscription usage endpoint. The HTTP usage
// endpoint is an internal ChatGPT API, not the OpenAI API billing service; keep
// usage_url configurable in case it changes.
const (
	openAIClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	openAIAuthorize   = "https://auth.openai.com/oauth/authorize"
	openAITokenURL    = "https://auth.openai.com/oauth/token"
	openAIRedirectURI = "http://localhost:1455/auth/callback"
	openAIUsageURL    = "https://chatgpt.com/backend-api/wham/usage"
)

type openAI struct {
	label, bearer, tokenFile, accountID string
	usageURL, authURL, tokenURL         string
	oauth                               *oauth.Manager
}

func newOpenAI(m config.Monitor, mgr *oauth.Manager) *openAI {
	usage := m.UsageURL
	if usage == "" {
		base := strings.TrimRight(m.BaseURL, "/")
		if base == "" {
			usage = openAIUsageURL
		} else {
			usage = base + "/wham/usage"
		}
	}
	authURL := m.AuthorizeURL
	if authURL == "" {
		authURL = openAIAuthorize
	}
	tokenURL := m.TokenURL
	if tokenURL == "" {
		tokenURL = openAITokenURL
	}
	return &openAI{
		label: m.Label, bearer: m.BearerToken, tokenFile: m.TokenFile,
		accountID: m.AccountID, usageURL: usage, authURL: authURL, tokenURL: tokenURL, oauth: mgr,
	}
}

func (o *openAI) ID() string    { return "openai" }
func (o *openAI) Label() string { return o.label }
func (o *openAI) Kind() string  { return model.KindSubscription }
func (o *openAI) Manual() bool  { return true }

func (o *openAI) OAuthConfig() oauth.Config {
	return oauth.Config{
		Provider: o.ID(), AuthorizeURL: o.authURL, TokenURL: o.tokenURL,
		ClientID: openAIClientID, RedirectURI: openAIRedirectURI,
		Scopes:      []string{"openid", "profile", "email", "offline_access"},
		FormEncoded: true,
		ExtraAuth: map[string]string{
			"id_token_add_organizations": "true",
			"codex_cli_simplified_flow":  "true",
			"originator":                 "codex_cli_rs",
		},
	}
}

func (o *openAI) Fetch(ctx context.Context) (model.Status, error) {
	st := model.Status{Provider: o.ID(), Label: o.label, Kind: o.Kind()}
	// External credentials are read on every poll, and never refreshed or
	// overwritten by aimon. The CLI owns refresh-token rotation for its file.
	token, accountID := o.bearer, ""
	if o.tokenFile != "" {
		var err error
		token, accountID, err = readCodexCredentials(o.tokenFile)
		if err != nil {
			return st, fmt.Errorf("OpenAI token_file: %w", err)
		}
	}
	if token == "" && o.oauth != nil {
		var err error
		token, err = o.oauth.AccessToken(ctx, o.label, o.OAuthConfig())
		if err != nil && !errors.Is(err, store.ErrNoToken) {
			st.NeedsAuth = true
			st.Error = "OpenAI auth could not be refreshed — reconnect"
			return st, fmt.Errorf("OpenAI auth: %w", err)
		}
	}
	if token == "" {
		st.NeedsAuth = true
		st.Error = "not connected — click Connect to authorize OpenAI with ChatGPT"
		return st, nil
	}
	if accountID == "" {
		accountID = openAIAccountID(token)
	}
	if o.accountID != "" {
		accountID = o.accountID
	}
	headers := map[string]string{"Authorization": "Bearer " + token}
	if accountID != "" {
		headers["ChatGPT-Account-Id"] = accountID
	}
	code, raw, err := getJSON(ctx, o.usageURL, headers, nil)
	if err != nil {
		return st, fmt.Errorf("OpenAI usage request: %w", err)
	}
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		switch {
		case o.tokenFile != "":
			st.Error = "OpenAI authorization failed — sign in again with Codex to update token_file"
		case o.bearer != "":
			st.Error = "OpenAI authorization failed — replace bearer_token with a ChatGPT OAuth access token"
		default:
			st.NeedsAuth = true
			st.Error = "OpenAI authorization failed — reconnect"
		}
		return st, fmt.Errorf("OpenAI usage HTTP %d", code)
	}
	if code != http.StatusOK {
		// Do not publish upstream bodies, which can contain account information.
		return st, fmt.Errorf("OpenAI usage HTTP %d", code)
	}
	return parseOpenAIUsage(o.label, raw, time.Now())
}

type codexCredentials struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	AccountID   string `json:"account_id"`
}

// readCodexCredentials accepts Codex's auth.json and a flat OAuth token object.
// Deliberately avoid a recursive token search: API keys and refresh/id tokens
// are not subscription access tokens.
func readCodexCredentials(path string) (token, accountID string, err error) {
	raw, err := os.ReadFile(expandHome(path))
	if err != nil {
		return "", "", err
	}
	var file struct {
		codexCredentials
		Tokens *codexCredentials `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return "", "", fmt.Errorf("invalid credentials JSON")
	}
	creds := file.codexCredentials
	if file.Tokens != nil {
		creds = *file.Tokens
	}
	if creds.AccessToken == "" {
		return "", "", fmt.Errorf("no access_token; sign in to Codex with ChatGPT (API-key login does not provide subscription usage)")
	}
	accountID = creds.AccountID
	if accountID == "" {
		accountID = openAIAccountID(creds.AccessToken)
	}
	if accountID == "" {
		accountID = openAIAccountID(creds.IDToken)
	}
	return creds.AccessToken, accountID, nil
}

// Only extract routing metadata from the caller's token; the upstream service
// validates its signature and authorization. Never expose the claims in Status.
func openAIAccountID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	return claims.Auth.AccountID
}

type openAIWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds int64    `json:"limit_window_seconds"`
	ResetAt            int64    `json:"reset_at"`
	ResetAfterSeconds  *int64   `json:"reset_after_seconds"`
}

type openAIRateLimit struct {
	Primary   *openAIWindow `json:"primary_window"`
	Secondary *openAIWindow `json:"secondary_window"`
}

func parseOpenAIUsage(label string, raw []byte, now time.Time) (model.Status, error) {
	st := model.Status{Provider: "openai", Label: label, Kind: model.KindSubscription, Unit: "%", Plan: "ChatGPT / Codex"}
	var usage struct {
		PlanType             string           `json:"plan_type"`
		RateLimit            *openAIRateLimit `json:"rate_limit"`
		CodeReviewRateLimit  *openAIRateLimit `json:"code_review_rate_limit"`
		AdditionalRateLimits []struct {
			LimitName      string           `json:"limit_name"`
			MeteredFeature string           `json:"metered_feature"`
			RateLimit      *openAIRateLimit `json:"rate_limit"`
		} `json:"additional_rate_limits"`
	}
	if err := json.Unmarshal(raw, &usage); err != nil {
		return st, fmt.Errorf("decode OpenAI usage: invalid response JSON")
	}
	if usage.PlanType != "" {
		st.Plan = "ChatGPT " + usage.PlanType + " / Codex"
	}
	var spans []model.Span
	appendLimits := func(limits *openAIRateLimit, suffix string) {
		if limits == nil {
			return
		}
		for i, window := range []*openAIWindow{limits.Primary, limits.Secondary} {
			if window == nil || window.UsedPercent == nil {
				continue
			}
			name := openAIWindowName(window.LimitWindowSeconds)
			if name == "" {
				name = []string{"primary", "secondary"}[i]
			}
			if suffix != "" {
				name += " " + suffix
			}
			reset := window.ResetAt
			if reset <= 0 && window.ResetAfterSeconds != nil && *window.ResetAfterSeconds >= 0 {
				reset = now.Unix() + *window.ResetAfterSeconds
			}
			spans = append(spans, model.Span{
				Name:     name,
				Used:     clampPct(*window.UsedPercent),
				ResetAt:  max(0, reset),
				Duration: max(0, window.LimitWindowSeconds),
			})
		}
	}
	appendLimits(usage.RateLimit, "")
	appendLimits(usage.CodeReviewRateLimit, "review")
	for _, limits := range usage.AdditionalRateLimits {
		name := limits.LimitName
		if name == "" {
			name = limits.MeteredFeature
		}
		if name == "" {
			name = "additional"
		}
		appendLimits(limits.RateLimit, name)
	}
	if len(spans) == 0 {
		return st, fmt.Errorf("OpenAI usage: no quota windows returned for this account")
	}
	applySpans(&st, spans)
	st.OK = true
	return st, nil
}

func openAIWindowName(seconds int64) string {
	if seconds <= 0 {
		return ""
	}
	for _, unit := range []struct {
		seconds int64
		label   string
	}{{604800, "w"}, {86400, "d"}, {3600, "h"}, {60, "m"}, {1, "s"}} {
		if seconds%unit.seconds == 0 {
			return fmt.Sprintf("%d%s", seconds/unit.seconds, unit.label)
		}
	}
	return ""
}
