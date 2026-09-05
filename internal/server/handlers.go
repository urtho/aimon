package server

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"time"

	"github.com/algonode/aimon/internal/providers"
	"github.com/algonode/aimon/internal/store"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// GET /api/status — latest snapshot of every monitor.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"monitors":      s.poller.Latest(),
		"poll_interval": int(s.cfg.PollInterval / time.Second),
		"server_time":   time.Now().Unix(),
	})
}

// GET /api/history?provider=&label=&hours= — sparkline series for one monitor.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	label := r.URL.Query().Get("label")
	if provider == "" || label == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider and label required"})
		return
	}
	hours := 24
	if h, err := strconv.Atoi(r.URL.Query().Get("hours")); err == nil && h > 0 && h <= 24*90 {
		hours = h
	}
	since := time.Now().Add(-time.Duration(hours) * time.Hour).Unix()
	points, err := s.store.History(r.Context(), provider, label, since, 3000)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if points == nil {
		points = []store.Point{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"points": points})
}

// GET /api/stream — Server-Sent Events; pushes the full status list on each poll.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)

	ch, cancel := s.poller.Subscribe()
	defer cancel()

	send := func(list any) bool {
		data, err := json.Marshal(list)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: status\ndata: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// Push the current state immediately so a fresh client isn't blank.
	if !send(s.poller.Latest()) {
		return
	}

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case snap, ok := <-ch:
			if !ok {
				return
			}
			if !send(snap) {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// oauthProvider resolves a monitor to its OAuthProvider, or nil.
func (s *Server) oauthProvider(provider, label string) providers.OAuthProvider {
	p, ok := s.provs[provider+"/"+label]
	if !ok {
		return nil
	}
	op, ok := p.(providers.OAuthProvider)
	if !ok {
		return nil
	}
	return op
}

// GET /oauth/start?provider=&label= — begin an OAuth flow, return the auth URL.
func (s *Server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	provider := r.URL.Query().Get("provider")
	label := r.URL.Query().Get("label")
	op := s.oauthProvider(provider, label)
	if op == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "monitor does not support OAuth"})
		return
	}
	cfg := op.OAuthConfig()
	redirectURI := cfg.RedirectURI
	if !op.Manual() && s.cfg.PublicURL != "" {
		redirectURI = s.cfg.PublicURL + "/oauth/callback"
	}
	authURL, state, err := s.oauth.StartFlow(r.Context(), label, cfg, redirectURI)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_url": authURL,
		"manual":   op.Manual(),
		"state":    state,
	})
}

type oauthCompleteReq struct {
	Provider string `json:"provider"`
	Label    string `json:"label"`
	Code     string `json:"code"`
	State    string `json:"state"`
}

// POST /oauth/complete — exchange the pasted code for tokens.
func (s *Server) handleOAuthComplete(w http.ResponseWriter, r *http.Request) {
	var req oauthCompleteReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	op := s.oauthProvider(req.Provider, req.Label)
	if op == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "monitor does not support OAuth"})
		return
	}
	if err := s.oauth.Complete(r.Context(), op.OAuthConfig(), req.State, req.Code); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.poller.PollNow() // reflect the new connection right away
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type oauthDisconnectReq struct {
	Provider string `json:"provider"`
	Label    string `json:"label"`
}

// POST /oauth/disconnect — forget stored tokens for a monitor.
func (s *Server) handleOAuthDisconnect(w http.ResponseWriter, r *http.Request) {
	var req oauthDisconnectReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if err := s.oauth.Disconnect(r.Context(), req.Provider, req.Label); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.poller.PollNow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// GET /auth/callback — Codex/OpenAI loopback redirect (listens on :1455).
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if errCode := q.Get("error"); errCode != "" {
		if state := q.Get("state"); state != "" {
			_, _ = s.store.TakePending(r.Context(), state)
		}
		msg := q.Get("error_description")
		if msg == "" {
			msg = errCode
		}
		writeCallbackPage(w, http.StatusBadRequest, "Authorization failed", msg, false)
		return
	}
	code := q.Get("code")
	state := q.Get("state")
	if code == "" || state == "" {
		writeCallbackPage(w, http.StatusBadRequest, "Authorization failed",
			"missing code or state in the callback", false)
		return
	}
	pending, err := s.store.GetPending(r.Context(), state)
	if err != nil {
		writeCallbackPage(w, http.StatusBadRequest, "Authorization failed",
			"this login attempt expired or was already used — click Connect again in aimon", false)
		return
	}
	op := s.oauthProvider(pending.Provider, pending.Label)
	if op == nil {
		writeCallbackPage(w, http.StatusBadRequest, "Authorization failed",
			"no matching monitor for this login", false)
		return
	}
	if err := s.oauth.Complete(r.Context(), op.OAuthConfig(), state, code); err != nil {
		s.log.Error("oauth callback exchange", "provider", pending.Provider, "label", pending.Label, "err", err)
		writeCallbackPage(w, http.StatusBadRequest, "Authorization failed",
			"could not complete OpenAI login — try Connect again", false)
		return
	}
	s.log.Info("oauth connected", "provider", pending.Provider, "label", pending.Label)
	s.poller.PollNow()
	writeCallbackPage(w, http.StatusOK, "Connected",
		"OpenAI is connected. You can close this tab and return to the aimon dashboard.", true)
}

func writeCallbackPage(w http.ResponseWriter, status int, title, message string, ok bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	color := "#ff5f56"
	if ok {
		color = "#33ff66"
	}
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>aimon · %s</title>
<style>
  html,body{height:100%%;margin:0;background:#050805;color:#33ff66;
    font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
  main{min-height:100%%;display:grid;place-items:center;padding:24px}
  .card{max-width:36rem;border:1px solid #1c3a1c;border-radius:4px;padding:28px 32px}
  h1{margin:0 0 12px;font-size:20px;color:%s}
  p{margin:0;color:#1f9d4f;line-height:1.5}
</style>
</head>
<body data-ok="%t">
<main><div class="card"><h1>%s</h1><p>%s</p></div></main>
</body>
</html>
`, html.EscapeString(title), color, ok, html.EscapeString(title), html.EscapeString(message))
}
