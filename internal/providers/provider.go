// Package providers implements one credit/usage fetcher per upstream service.
// Each Provider turns an account's credentials into a normalized model.Status.
package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/algonode/aimon/internal/config"
	"github.com/algonode/aimon/internal/model"
	"github.com/algonode/aimon/internal/oauth"
)

// Provider fetches the status of a single monitored account.
type Provider interface {
	ID() string    // provider type, e.g. "deepseek"
	Label() string // account label
	Kind() string  // model.Kind*
	// Fetch returns a populated Status on success. On failure it returns a
	// non-nil error; the poller synthesizes an error Status from the metadata.
	Fetch(ctx context.Context) (model.Status, error)
}

// sharedClient is reused across providers (connection pooling, sane timeout).
var sharedClient = &http.Client{Timeout: 25 * time.Second}

// Build constructs the Provider for a monitor config, wiring the OAuth manager
// for providers that need it.
func Build(m config.Monitor, oauthMgr *oauth.Manager) (Provider, error) {
	switch m.Provider {
	case "deepseek":
		return newDeepSeek(m), nil
	case "openrouter":
		return newOpenRouter(m), nil
	case "anthropic":
		return newAnthropic(m), nil
	case "claude":
		return newClaude(m, oauthMgr), nil
	case "xai", "x.ai", "grok":
		return newXAI(m, oauthMgr), nil
	default:
		return nil, fmt.Errorf("unknown provider %q", m.Provider)
	}
}

// getJSON performs a GET and decodes a JSON body into out. It returns the HTTP
// status and the raw body so callers can surface upstream error messages.
func getJSON(ctx context.Context, urlStr string, headers map[string]string, out any) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := sharedClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, raw, fmt.Errorf("decode %s: %w", urlStr, err)
		}
	}
	return resp.StatusCode, raw, nil
}

// postJSON performs a POST with a JSON body and decodes a JSON response into
// out. Used for Connect-protocol RPC endpoints (e.g. grok.com billing).
func postJSON(ctx context.Context, urlStr string, headers map[string]string, reqBody any, out any) (int, []byte, error) {
	var buf []byte
	if reqBody != nil {
		var err error
		if buf, err = json.Marshal(reqBody); err != nil {
			return 0, nil, err
		}
	} else {
		buf = []byte("{}")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, urlStr, bytes.NewReader(buf))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := sharedClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, raw, fmt.Errorf("decode %s: %w", urlStr, err)
		}
	}
	return resp.StatusCode, raw, nil
}

// fptr returns a pointer to f — for the optional numeric Status fields.
func fptr(f float64) *float64 { return &f }

// snippet trims an upstream body for inclusion in an error message.
func snippet(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
