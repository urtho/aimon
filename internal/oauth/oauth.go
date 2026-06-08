// Package oauth implements a small OAuth2 Authorization-Code-with-PKCE manager.
// It supports the "manual" paste-the-code flow used by Anthropic/Claude (the
// consent page redirects to a callback that displays `code#state`, which the
// user pastes back) as well as a normal browser redirect callback. Tokens and
// in-flight PKCE verifiers are persisted via the store so they survive restarts.
package oauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/algonode/aimon/internal/store"
)

// Config describes a provider's OAuth endpoints and client.
type Config struct {
	Provider     string
	AuthorizeURL string
	TokenURL     string
	ClientID     string
	ClientSecret string            // optional (public clients use PKCE only)
	Scopes       []string          // space-joined into the scope param
	RedirectURI  string            // default redirect if the caller passes none
	FormEncoded  bool              // true => application/x-www-form-urlencoded; false => JSON
	ExtraAuth    map[string]string // extra params appended to the authorize URL
	// Nonce adds a random `nonce` to the authorize request (OIDC; x.ai requires it).
	Nonce bool
	// ChallengeOnExchange re-sends code_challenge/method in the token request
	// (some servers, e.g. auth.x.ai, expect it alongside code_verifier).
	ChallengeOnExchange bool
	// ParamOrder pins the authorize query-param order to match a reference client
	// byte-for-byte (Anthropic's flow is order-sensitive in practice). Keys not
	// listed are appended sorted. Empty => fully sorted.
	ParamOrder []string
}

// Manager runs OAuth flows and hands out fresh access tokens.
type Manager struct {
	store  *store.Store
	client *http.Client
}

// New constructs a Manager backed by the given store.
func New(s *store.Store) *Manager {
	// The token endpoints (notably platform.claude.com) can be slow under load;
	// give the exchange/refresh generous headroom.
	return &Manager{store: s, client: &http.Client{Timeout: 60 * time.Second}}
}

// tokenResponse is the standard OAuth2 token endpoint payload.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// StartFlow generates PKCE material, persists it keyed by a fresh state value,
// and returns the authorize URL the user should open.
func (m *Manager) StartFlow(ctx context.Context, label string, cfg Config, redirectURI string) (authURL, state string, err error) {
	if redirectURI == "" {
		redirectURI = cfg.RedirectURI
	}
	verifier, err := randomB64(32)
	if err != nil {
		return "", "", err
	}
	state, err = randomB64(16)
	if err != nil {
		return "", "", err
	}
	challenge := s256Challenge(verifier)

	if err := m.store.SavePending(ctx, state, cfg.Provider, label, verifier, redirectURI); err != nil {
		return "", "", fmt.Errorf("persist pending flow: %w", err)
	}

	params := map[string]string{
		"response_type":         "code",
		"client_id":             cfg.ClientID,
		"redirect_uri":          redirectURI,
		"state":                 state,
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
	}
	if len(cfg.Scopes) > 0 {
		params["scope"] = strings.Join(cfg.Scopes, " ")
	}
	if cfg.Nonce {
		nonce, nerr := randomB64(16)
		if nerr != nil {
			return "", "", nerr
		}
		params["nonce"] = nonce
	}
	for k, v := range cfg.ExtraAuth {
		params[k] = v
	}

	sep := "?"
	if strings.Contains(cfg.AuthorizeURL, "?") {
		sep = "&"
	}
	return cfg.AuthorizeURL + sep + encodeOrdered(params, cfg.ParamOrder), state, nil
}

// encodeOrdered builds a query string with the keys in `order` first (in that
// order), then any remaining keys sorted. Spaces are encoded as '+' via
// url.QueryEscape — matching the reference OAuth clients (claude-code uses '+',
// not '%20', and is order-sensitive).
func encodeOrdered(params map[string]string, order []string) string {
	var b strings.Builder
	seen := make(map[string]bool, len(params))
	write := func(k string) {
		v, ok := params[k]
		if !ok || seen[k] {
			return
		}
		seen[k] = true
		if b.Len() > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(k))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(v))
	}
	for _, k := range order {
		write(k)
	}
	rest := make([]string, 0, len(params))
	for k := range params {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		write(k)
	}
	return b.String()
}

// Complete exchanges an authorization code for tokens and stores them. The code
// may be the raw value or the `code#state` form produced by the manual flow; in
// the latter case the embedded state overrides the state argument.
func (m *Manager) Complete(ctx context.Context, cfg Config, state, code string) error {
	code, state = parsePastedCode(code, state)

	pending, err := m.store.TakePending(ctx, state)
	if err != nil {
		return err
	}

	body := map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  pending.RedirectURI,
		"client_id":     cfg.ClientID,
		"code_verifier": pending.CodeVerifier,
		"state":         state,
	}
	if cfg.ClientSecret != "" {
		body["client_secret"] = cfg.ClientSecret
	}
	if cfg.ChallengeOnExchange {
		body["code_challenge"] = s256Challenge(pending.CodeVerifier)
		body["code_challenge_method"] = "S256"
	}

	tok, err := m.exchange(ctx, cfg, body)
	if err != nil {
		return err
	}
	return m.persist(ctx, cfg.Provider, pending.Label, tok)
}

// AccessToken returns a valid access token for the monitor, transparently
// refreshing it when it has expired and a refresh token is available.
func (m *Manager) AccessToken(ctx context.Context, label string, cfg Config) (string, error) {
	t, err := m.store.GetToken(ctx, cfg.Provider, label)
	if err != nil {
		return "", err
	}
	if !t.Expired(60 * time.Second) {
		return t.AccessToken, nil
	}
	if t.RefreshToken == "" {
		return "", fmt.Errorf("token expired and no refresh token; reconnect required")
	}

	body := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": t.RefreshToken,
		"client_id":     cfg.ClientID,
	}
	if cfg.ClientSecret != "" {
		body["client_secret"] = cfg.ClientSecret
	}
	tok, err := m.exchange(ctx, cfg, body)
	if err != nil {
		return "", fmt.Errorf("refresh token: %w", err)
	}
	// Some providers omit a new refresh token on refresh; keep the old one.
	if tok.RefreshToken == "" {
		tok.RefreshToken = t.RefreshToken
	}
	if err := m.persist(ctx, cfg.Provider, label, tok); err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

// HasToken reports whether a token is stored for the monitor.
func (m *Manager) HasToken(ctx context.Context, provider, label string) bool {
	_, err := m.store.GetToken(ctx, provider, label)
	return err == nil
}

// Disconnect removes a stored token.
func (m *Manager) Disconnect(ctx context.Context, provider, label string) error {
	return m.store.DeleteToken(ctx, provider, label)
}

func (m *Manager) exchange(ctx context.Context, cfg Config, body map[string]string) (*tokenResponse, error) {
	var req *http.Request
	var err error
	if cfg.FormEncoded {
		form := url.Values{}
		for k, v := range body {
			form.Set(k, v)
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL, strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	} else {
		buf, _ := json.Marshal(body)
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL, bytes.NewReader(buf))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var tr tokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return nil, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if resp.StatusCode >= 400 || tr.Error != "" {
		msg := tr.ErrorDesc
		if msg == "" {
			msg = tr.Error
		}
		if msg == "" {
			msg = truncate(string(raw), 200)
		}
		return nil, fmt.Errorf("token endpoint %d: %s", resp.StatusCode, msg)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("token endpoint returned no access_token")
	}
	return &tr, nil
}

func (m *Manager) persist(ctx context.Context, provider, label string, tok *tokenResponse) error {
	var expiresAt int64
	if tok.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	rawJSON, _ := json.Marshal(tok)
	return m.store.SaveToken(ctx, store.Token{
		Provider:     provider,
		Label:        label,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    expiresAt,
		Scope:        tok.Scope,
		Raw:          string(rawJSON),
	})
}

// parsePastedCode extracts the authorization code and state from whatever the
// user pastes back: a bare code, Anthropic's `code#state`, a `code=…&state=…`
// query string, or a full redirect URL (x.ai's loopback callback). fallback is
// the state we generated, used when the pasted value doesn't carry one.
func parsePastedCode(input, fallback string) (code, state string) {
	input = strings.TrimSpace(input)
	state = strings.TrimSpace(fallback)

	// Full URL or bare query string carrying code=…&state=… (x.ai redirect).
	if strings.Contains(input, "code=") {
		qs := input
		if i := strings.IndexAny(input, "?#"); i >= 0 {
			qs = input[i+1:]
		}
		if v, err := url.ParseQuery(qs); err == nil {
			if c := v.Get("code"); c != "" {
				if s := v.Get("state"); s != "" {
					state = s
				}
				return c, state
			}
		}
	}

	// Anthropic-style code#state.
	if i := strings.IndexByte(input, '#'); i >= 0 {
		if s := strings.TrimSpace(input[i+1:]); s != "" {
			state = s
		}
		return strings.TrimSpace(input[:i]), state
	}

	return input, state
}

// randomB64 returns n random bytes as a base64url (no padding) string.
func randomB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// s256Challenge computes the PKCE S256 challenge for a verifier.
func s256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
