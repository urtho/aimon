package server

import (
	"fmt"
	"html"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/algonode/aimon/internal/model"
)

// waybarPayload is the object Waybar custom modules consume when
// `return-type` is `json`. Extra fields (monitors) are ignored by Waybar
// and exist for scripts that parse the same URL.
type waybarPayload struct {
	Text       string          `json:"text"`
	Tooltip    string          `json:"tooltip"`
	Class      string          `json:"class"`
	Percentage *int            `json:"percentage,omitempty"`
	Alt        string          `json:"alt"`
	Monitors   []waybarMonitor `json:"monitors"`
}

// waybarMonitor is one compact row a gadget can render on its own.
type waybarMonitor struct {
	Provider   string       `json:"provider"`
	Label      string       `json:"label"`
	Kind       string       `json:"kind"`
	OK         bool         `json:"ok"`
	NeedsAuth  bool         `json:"needs_auth,omitempty"`
	Error      string       `json:"error,omitempty"`
	Text       string       `json:"text"`
	Detail     string       `json:"detail"`
	Class      string       `json:"class"`
	Pace       string       `json:"pace,omitempty"` // below | at | over vs linear spend
	Percentage *int         `json:"percentage,omitempty"`
	Plan       string       `json:"plan,omitempty"`
	Spans      []model.Span `json:"spans,omitempty"`
	UpdatedAt  int64        `json:"updated_at,omitempty"`
}

const (
	waybarClassEmpty    = "empty"
	waybarClassOK       = "ok"
	waybarClassWarn     = "warn"
	waybarClassCritical = "critical"
	waybarClassAuth     = "auth"
	waybarClassError    = "error"
)

var waybarClassRank = map[string]int{
	waybarClassEmpty:    0,
	waybarClassOK:       1,
	model.PaceBelow:     1,
	model.PaceAt:        2,
	waybarClassWarn:     2,
	model.PaceOver:      3,
	waybarClassCritical: 3,
	waybarClassAuth:     4,
	waybarClassError:    5,
}

// Pango colors for Waybar `markup: pango`. Match ~/.config/waybar/style.css.
var waybarPaceColor = map[string]string{
	model.PaceBelow:  "#7dffb3",
	model.PaceAt:     "#ffcc66",
	model.PaceOver:   "#ff5555",
	waybarClassAuth:  "#ffcc66",
	waybarClassError: "#ff5555",
}

var waybarIcons = map[string]string{
	"claude":     "C",
	"openai":     "AI",
	"xai":        "X",
	"deepseek":   "DS",
	"openrouter": "OR",
	"anthropic":  "AN",
}

// GET /api/waybar — Waybar custom-module JSON for the latest snapshots.
// Subscription compact % / pace / tooltip use weekly quota windows; a monitor
// with no weekly window (e.g. a Claude Team seat with only a 5h session cap)
// falls back to its session windows. Optional filters: provider,
// label, kind (comma-separated, case-insensitive).
func (s *Server) handleWaybar(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	all := s.poller.Latest()
	// Icons are numbered over the unfiltered list so a second Claude seat stays
	// "C2" even when the gadget filters down to just that label.
	icons := monitorIcons(all)
	list := filterStatuses(all, q.Get("provider"), q.Get("label"), q.Get("kind"))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, formatWaybar(list, icons, time.Now()))
}

// monitorIcons maps each monitor ID to its compact bar token. The first
// monitor of a provider (config order) gets the plain icon ("C"); later ones
// are numbered ("C2", "C3") so multiple seats/logins stay distinguishable.
func monitorIcons(list []model.Status) map[string]string {
	out := make(map[string]string, len(list))
	seen := make(map[string]int)
	for _, st := range list {
		prov := strings.ToLower(st.Provider)
		seen[prov]++
		icon := providerIcon(st.Provider)
		if n := seen[prov]; n > 1 {
			icon += strconv.Itoa(n)
		}
		out[st.ID()] = icon
	}
	return out
}

func filterStatuses(list []model.Status, provider, label, kind string) []model.Status {
	wantProv := csvSet(provider)
	wantLabel := csvSet(label)
	wantKind := csvSet(kind)
	if len(wantProv) == 0 && len(wantLabel) == 0 && len(wantKind) == 0 {
		return list
	}
	out := make([]model.Status, 0, len(list))
	for _, st := range list {
		if len(wantProv) > 0 {
			if _, ok := wantProv[strings.ToLower(st.Provider)]; !ok {
				continue
			}
		}
		if len(wantLabel) > 0 {
			if _, ok := wantLabel[strings.ToLower(st.Label)]; !ok {
				continue
			}
		}
		if len(wantKind) > 0 {
			if _, ok := wantKind[strings.ToLower(st.Kind)]; !ok {
				continue
			}
		}
		out = append(out, st)
	}
	return out
}

func csvSet(s string) map[string]struct{} {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := make(map[string]struct{})
	for _, p := range strings.Split(s, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			out[p] = struct{}{}
		}
	}
	return out
}

// formatWaybar renders list; icons (from monitorIcons) may be nil, in which
// case tokens are numbered over list itself.
func formatWaybar(list []model.Status, icons map[string]string, now time.Time) waybarPayload {
	if icons == nil {
		icons = monitorIcons(list)
	}
	out := waybarPayload{
		Monitors: make([]waybarMonitor, 0, len(list)),
	}
	if len(list) == 0 {
		out.Text = "aimon"
		out.Tooltip = "no monitors"
		out.Class = waybarClassEmpty
		out.Alt = waybarClassEmpty
		return out
	}

	texts := make([]string, 0, len(list))
	details := make([]string, 0, len(list))
	class := "" // rank 0; first real class always wins
	var minPct *int
	for _, st := range list {
		bars := barSpans(st.Spans)
		pct := remainingPct(st)
		cls := monitorClass(st, now)
		text := compactText(st, icons[st.ID()], pct)
		detail := monitorDetail(st, now)
		pace, _ := barPace(st, now)
		wm := waybarMonitor{
			Provider:   st.Provider,
			Label:      st.Label,
			Kind:       st.Kind,
			OK:         st.OK,
			NeedsAuth:  st.NeedsAuth,
			Error:      st.Error,
			Text:       text,
			Detail:     detail,
			Class:      cls,
			Pace:       pace,
			Percentage: pct,
			Plan:       st.Plan,
			Spans:      bars,
			UpdatedAt:  st.UpdatedAt,
		}
		out.Monitors = append(out.Monitors, wm)
		texts = append(texts, colorize(text, cls))
		details = append(details, detail)
		if waybarClassRank[cls] > waybarClassRank[class] {
			class = cls
		}
		if st.OK && pct != nil && (minPct == nil || *pct < *minPct) {
			v := *pct
			minPct = &v
		}
	}
	if class == "" {
		class = waybarClassOK
	}
	out.Text = strings.Join(texts, " · ")
	out.Tooltip = strings.Join(details, "\n")
	out.Class = class
	out.Alt = class
	out.Percentage = minPct
	return out
}

// barSpans picks the windows that drive the bar: weekly windows when the
// monitor has any, otherwise every window with a known length (session caps).
// Unwindowed meters (extra credits) never drive it.
func barSpans(spans []model.Span) []model.Span {
	weekly := make([]model.Span, 0, len(spans))
	windowed := make([]model.Span, 0, len(spans))
	for _, sp := range spans {
		if sp.Weekly() {
			weekly = append(weekly, sp)
		}
		if sp.WindowSeconds() > 0 {
			windowed = append(windowed, sp)
		}
	}
	if len(weekly) > 0 {
		return weekly
	}
	return windowed
}

// remainingPct is remaining quota in the bar windows (see barSpans).
func remainingPct(s model.Status) *int {
	if !s.OK {
		return nil
	}
	if left, ok := tightestRemaining(barSpans(s.Spans)); ok {
		return clampPctPtr(int(math.Round(left)))
	}
	if len(s.Spans) > 0 {
		return nil
	}
	if r, ok := s.Remaining(); ok {
		v := int(math.Round(r * 100))
		return clampPctPtr(v)
	}
	if s.Kind == model.KindSubscription && s.Balance != nil {
		return clampPctPtr(int(math.Round(*s.Balance)))
	}
	return nil
}

func tightestRemaining(spans []model.Span) (float64, bool) {
	if len(spans) == 0 {
		return 0, false
	}
	minLeft := 100 - spans[0].Used
	for _, sp := range spans[1:] {
		if left := 100 - sp.Used; left < minLeft {
			minLeft = left
		}
	}
	return minLeft, true
}

func clampPctPtr(v int) *int {
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return &v
}

func monitorClass(s model.Status, now time.Time) string {
	if s.NeedsAuth {
		return waybarClassAuth
	}
	if !s.OK {
		return waybarClassError
	}
	if p, ok := barPace(s, now); ok {
		return p
	}
	return waybarClassOK
}

func barPace(s model.Status, now time.Time) (string, bool) {
	tmp := s
	tmp.Spans = barSpans(s.Spans)
	return tmp.WorstPace(now)
}

func colorize(text, class string) string {
	color, ok := waybarPaceColor[class]
	if !ok {
		return text
	}
	return fmt.Sprintf(`<span color="%s">%s</span>`, color, html.EscapeString(text))
}

func compactText(s model.Status, icon string, pct *int) string {
	if icon == "" {
		icon = providerIcon(s.Provider)
	}
	switch {
	case s.NeedsAuth:
		return icon + " ?"
	case !s.OK:
		return icon + " !"
	case s.Kind == model.KindSubscription:
		if pct != nil {
			return fmt.Sprintf("%s %d%%", icon, *pct)
		}
		return icon + " ok"
	case s.Kind == model.KindCost:
		if s.Used != nil {
			return icon + " " + fmtMoney(*s.Used, s.Unit)
		}
		return icon + " ok"
	default:
		if s.Balance != nil {
			return icon + " " + fmtMoney(*s.Balance, s.Unit)
		}
		if s.Used != nil {
			return icon + " " + fmtMoney(*s.Used, s.Unit)
		}
		return icon + " ok"
	}
}

func monitorDetail(s model.Status, now time.Time) string {
	head := s.Label
	if s.OK && s.Plan != "" {
		head += "  " + s.Plan
	}
	if s.NeedsAuth {
		msg := s.Error
		if msg == "" {
			msg = "not connected"
		}
		return head + "\n  " + msg
	}
	if !s.OK {
		msg := s.Error
		if msg == "" {
			msg = "error"
		}
		return head + "\n  " + msg
	}
	if bars := barSpans(s.Spans); len(bars) > 0 {
		var b strings.Builder
		b.WriteString(head)
		for _, sp := range bars {
			left := max(0, int(math.Round(100-sp.Used)))
			fmt.Fprintf(&b, "\n  %d%%/%s", left, sp.Name)
			if sp.ResetAt > 0 {
				fmt.Fprintf(&b, "  resets %s", fmtDur(sp.ResetAt-now.Unix()))
			}
			if class, expected, ok := sp.Pace(now); ok {
				linLeft := max(0, int(math.Round(100-expected)))
				fmt.Fprintf(&b, "  %s · linear %d%% left", class, linLeft)
			}
		}
		return b.String()
	}
	switch s.Kind {
	case model.KindSubscription:
		if pct := remainingPct(s); pct != nil {
			return head + fmt.Sprintf("\n  %d%% quota left", *pct)
		}
		return head + "\n  connected"
	case model.KindCost:
		line := head + "\n  "
		if s.Used != nil {
			line += fmtMoney(*s.Used, s.Unit) + " spent"
		} else {
			line += "ok"
		}
		if s.Window != "" {
			line += "  " + s.Window
		}
		return line
	default:
		line := head + "\n  "
		if s.Balance != nil {
			line += fmtMoney(*s.Balance, s.Unit) + " left"
		} else {
			line += "ok"
		}
		return line
	}
}

func providerIcon(provider string) string {
	if icon, ok := waybarIcons[strings.ToLower(provider)]; ok {
		return icon
	}
	p := strings.TrimSpace(provider)
	if p == "" {
		return "?"
	}
	if len(p) == 1 {
		return strings.ToUpper(p)
	}
	return strings.ToUpper(p[:2])
}

func fmtDur(sec int64) string {
	if sec <= 0 {
		return "now"
	}
	d := sec / 86400
	h := (sec % 86400) / 3600
	m := (sec % 3600) / 60
	switch {
	case d > 0:
		if h > 0 {
			return fmt.Sprintf("%dd%dh", d, h)
		}
		return fmt.Sprintf("%dd", d)
	case h > 0:
		if m > 0 {
			return fmt.Sprintf("%dh%02dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	default:
		if m < 1 {
			m = 1
		}
		return fmt.Sprintf("%dm", m)
	}
}

func fmtMoney(n float64, unit string) string {
	prec := 2
	if math.Abs(n) >= 1000 {
		prec = 0
	}
	v := strconv.FormatFloat(n, 'f', prec, 64)
	switch unit {
	case "USD":
		return "$" + v
	case "CNY":
		return "¥" + v
	case "EUR":
		return "€" + v
	case "GBP":
		return "£" + v
	case "", "%":
		return v
	default:
		return v + " " + unit
	}
}
