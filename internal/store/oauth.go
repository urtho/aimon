package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNoToken is returned by GetToken when no token is stored for the monitor.
var ErrNoToken = errors.New("no oauth token")

// Token is a persisted OAuth credential set for a (provider, label).
type Token struct {
	Provider     string
	Label        string
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // unix seconds, 0 = unknown/never
	Scope        string
	Raw          string // full token response JSON for debugging
	UpdatedAt    int64
}

// Expired reports whether the access token is past (or within skew of) expiry.
func (t Token) Expired(skew time.Duration) bool {
	if t.ExpiresAt == 0 {
		return false
	}
	return time.Now().Add(skew).Unix() >= t.ExpiresAt
}

// SaveToken upserts an OAuth token.
func (s *Store) SaveToken(ctx context.Context, t Token) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO oauth_tokens (provider, label, access_token, refresh_token, expires_at, scope, raw, updated_at)
VALUES (?,?,?,?,?,?,?,?)
ON CONFLICT(provider, label) DO UPDATE SET
    access_token=excluded.access_token,
    refresh_token=excluded.refresh_token,
    expires_at=excluded.expires_at,
    scope=excluded.scope,
    raw=excluded.raw,
    updated_at=excluded.updated_at`,
		t.Provider, t.Label, t.AccessToken, t.RefreshToken, t.ExpiresAt, t.Scope, t.Raw, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	return nil
}

// GetToken loads the OAuth token for a monitor, or ErrNoToken.
func (s *Store) GetToken(ctx context.Context, provider, label string) (Token, error) {
	var t Token
	var refresh, scope, raw sql.NullString
	var expires sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
SELECT provider, label, access_token, refresh_token, expires_at, scope, raw, updated_at
FROM oauth_tokens WHERE provider = ? AND label = ?`, provider, label).
		Scan(&t.Provider, &t.Label, &t.AccessToken, &refresh, &expires, &scope, &raw, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrNoToken
	}
	if err != nil {
		return Token{}, fmt.Errorf("get token: %w", err)
	}
	t.RefreshToken = refresh.String
	t.ExpiresAt = expires.Int64
	t.Scope = scope.String
	t.Raw = raw.String
	return t, nil
}

// DeleteToken removes a stored token (disconnect).
func (s *Store) DeleteToken(ctx context.Context, provider, label string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE provider = ? AND label = ?`, provider, label)
	return err
}

// SavePending stores a PKCE code_verifier keyed by the OAuth state parameter
// while the user completes the consent step.
func (s *Store) SavePending(ctx context.Context, state, provider, label, verifier, redirectURI string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO oauth_pending (state, provider, label, code_verifier, redirect_uri, created_at)
VALUES (?,?,?,?,?,?)
ON CONFLICT(state) DO UPDATE SET
    provider=excluded.provider, label=excluded.label,
    code_verifier=excluded.code_verifier, redirect_uri=excluded.redirect_uri,
    created_at=excluded.created_at`,
		state, provider, label, verifier, redirectURI, time.Now().Unix())
	return err
}

// Pending is an in-flight OAuth authorization.
type Pending struct {
	State        string
	Provider     string
	Label        string
	CodeVerifier string
	RedirectURI  string
}

// TakePending atomically fetches and deletes the pending flow for state.
func (s *Store) TakePending(ctx context.Context, state string) (Pending, error) {
	var p Pending
	err := s.db.QueryRowContext(ctx, `
SELECT state, provider, label, code_verifier, redirect_uri
FROM oauth_pending WHERE state = ?`, state).
		Scan(&p.State, &p.Provider, &p.Label, &p.CodeVerifier, &p.RedirectURI)
	if errors.Is(err, sql.ErrNoRows) {
		return Pending{}, fmt.Errorf("unknown or expired oauth state")
	}
	if err != nil {
		return Pending{}, err
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM oauth_pending WHERE state = ?`, state)
	return p, nil
}
