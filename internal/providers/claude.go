package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
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

	// Last good usage snapshot, reused across transient endpoint hiccups so the
	// card doesn't flap between window rows and a bare "connected" every time a
	// single poll 429s/times out. Fetch is never called concurrently for the
	// same monitor (the poller runs one fetch per provider per tick), so plain
	// fields are fine.
	lastSpans   []model.Span
	lastSpansAt time.Time
}

// staleSpanTTL is how long the last good usage windows keep being shown when
// the usage endpoint fails transiently.
const staleSpanTTL = 15 * time.Minute

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
		// Record the cause so /api/status shows WHY usage is missing.
		if code != 0 {
			base.Extra["usage_http"] = code
		}
		if ferr != nil {
			base.Extra["usage_err"] = ferr.Error()
		}
		if c.applyLastSpans(&base) {
			return base, nil // bridge the hiccup with the last good windows
		}
		base.Extra["usage"] = "unavailable"
		base.Extra["hint"] = "set `usage_url` for this monitor if your plan exposes a usage endpoint"
		return base, nil
	}

	if spans := parseClaudeUsage(rawAny); len(spans) > 0 {
		c.lastSpans, c.lastSpansAt = spans, time.Now()
		applySpans(&base, spans)
	} else {
		// 200 but nothing recognizable in the body.
		base.Extra["usage"] = "unparsed"
		c.applyLastSpans(&base)
	}
	// Keep a compact copy of the raw response for debugging in the UI.
	if b, mErr := json.Marshal(rawAny); mErr == nil {
		base.Extra["raw"] = string(b)
	}
	return base, nil
}

// applySpans fills a Status's Spans plus the aggregate fields, which track the
// tightest quota window (drives the summary, Waybar %, and DB history).
// Spans with no window length (extra credits, unnamed meters) stay as rows
// but do not drive Used/Balance.
func applySpans(st *model.Status, spans []model.Span) {
	st.Spans = spans
	if len(spans) == 0 {
		return
	}
	worst, ok := tightestSpan(spans, true)
	if !ok {
		worst, _ = tightestSpan(spans, false)
	}
	st.Used = fptr(worst.Used)
	st.Total = fptr(100)
	st.Balance = fptr(100 - worst.Used)
	if worst.ResetAt > 0 {
		r := worst.ResetAt
		st.ResetAt = &r
	}
}

func tightestSpan(spans []model.Span, windowedOnly bool) (model.Span, bool) {
	var worst model.Span
	found := false
	for _, sp := range spans {
		if windowedOnly && sp.WindowSeconds() <= 0 {
			continue
		}
		if !found || sp.Used > worst.Used {
			worst, found = sp, true
		}
	}
	return worst, found
}

// applyLastSpans reuses the last good windows when they're still fresh,
// marking the status as stale. Reports whether it applied anything.
func (c *claude) applyLastSpans(st *model.Status) bool {
	if len(c.lastSpans) == 0 || time.Since(c.lastSpansAt) > staleSpanTTL {
		return false
	}
	applySpans(st, c.lastSpans)
	st.Extra["usage"] = fmt.Sprintf("stale (%ds old)", int(time.Since(c.lastSpansAt).Seconds()))
	return true
}

// claudeWindowLabels maps the usage endpoint's window keys to short row labels
// and a display order (shortest window first).
var claudeWindowLabels = map[string]struct {
	label    string
	order    int
	duration int64
}{
	"five_hour":            {"5h", 0, 5 * 3600},
	"seven_day":            {"1w", 1, 7 * 24 * 3600},
	"seven_day_opus":       {"1w opus", 2, 7 * 24 * 3600},
	"seven_day_sonnet":     {"1w sonnet", 3, 7 * 24 * 3600},
	"seven_day_oauth_apps": {"1w apps", 4, 7 * 24 * 3600},
}

// parseClaudeUsage extracts the per-window quota spans from the OAuth usage
// endpoint. The response holds one object per limit window (five_hour,
// seven_day, seven_day_opus, ...), each with a `utilization` PERCENTAGE plus
// `resets_at`.
//
// utilization is always a percent (0-100). Do NOT re-scale values ≤ 1: a
// freshly reset window reporting e.g. 0.8 means 0.8%, and interpreting it as a
// 0..1 fraction made the gauge spike to ~100% whenever any window dipped
// below 1% — the "alternating 100% used" bug.
func parseClaudeUsage(m map[string]any) []model.Span {
	type entry struct {
		span  model.Span
		order int
	}
	var entries []entry

	var scan func(name string, v any)
	scan = func(name string, v any) {
		switch t := v.(type) {
		case map[string]any:
			if skipClaudeUsageObject(name, t) {
				return
			}
			if u, uok := toFloat(t["utilization"]); uok {
				label, order, dur := name, 100, int64(0) // unknown windows: raw key, listed last
				if def, known := claudeWindowLabels[name]; known {
					label, order, dur = def.label, def.order, def.duration
				}
				if dur == 0 {
					dur = model.ParseWindowDuration(label)
				}
				sp := model.Span{Name: label, Used: clampPct(u), Duration: dur}
				if r, rok := toUnix(t["resets_at"]); rok {
					sp.ResetAt = r
				}
				entries = append(entries, entry{sp, order})
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

	// When the payload has real plan windows, drop leftover meters that are
	// not a known window and have no duration/reset (nimbus_quill, etc.).
	hasKnown := false
	for _, e := range entries {
		if e.order < 100 {
			hasKnown = true
			break
		}
	}
	if hasKnown {
		n := 0
		for _, e := range entries {
			if e.order < 100 || e.span.Duration > 0 || e.span.ResetAt > 0 {
				entries[n] = e
				n++
			}
		}
		entries = entries[:n]
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].order != entries[j].order {
			return entries[i].order < entries[j].order
		}
		return entries[i].span.Name < entries[j].span.Name
	})
	spans := make([]model.Span, len(entries))
	for i, e := range entries {
		spans[i] = e.span
	}
	return spans
}

// skipClaudeUsageObject drops extra-credit / spend objects. They carry a
// `utilization` percentage (e.g. extra_usage 76% of purchased credits) that is
// not plan quota; treating them as spans made Waybar show 24% while 5h/1w
// were 100% remaining.
func skipClaudeUsageObject(name string, t map[string]any) bool {
	key := name
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		key = name[i+1:]
	}
	switch key {
	case "extra_usage", "spend":
		return true
	}
	if _, ok := t["used_credits"]; ok {
		return true
	}
	if _, ok := t["credits_ever_enabled"]; ok {
		return true
	}
	if _, ok := t["monthly_limit"]; ok {
		return true
	}
	return false
}

func clampPct(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 100 {
		return 100
	}
	return f
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
