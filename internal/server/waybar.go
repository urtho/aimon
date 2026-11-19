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
	// Windows are the values the bar shows for a healthy subscription: the
	// short window, then the long one, each with its own pace class. Gadgets
	// (apcmon) read these instead of redoing the quota math.
	Windows   []waybarWindow `json:"windows,omitempty"`
	UpdatedAt int64          `json:"updated_at,omitempty"`
}

// waybarWindow is one bar value: group "short" (<1w), "long" (>=1w) or
// "balance" (a subscription with no timed window).
//
// Window is the tightest span's length ("5h", "7d"), ResetAt its reset (unix
// s), LinearLeft the % a straight-line burn would leave now and Delta the
// points ahead of that line (linear used − used, worst span of the group:
// negative = burning faster). The last three are absent with no reset time;
// Delta is 0 for an untouched window without one.
type waybarWindow struct {
	Group      string `json:"group"`
	Left       int    `json:"left"`
	Class      string `json:"class,omitempty"`
	Window     string `json:"window,omitempty"`
	ResetAt    int64  `json:"reset_at,omitempty"`
	LinearLeft *int   `json:"linear_left,omitempty"`
	Delta      *int   `json:"delta,omitempty"`
}

const (
	waybarClassEmpty    = "empty"
	waybarClassOK       = "ok"
	waybarClassWarn     = "warn"
	waybarClassCritical = "critical"
	waybarClassAuth     = "auth"
	waybarClassError    = "error"

	// waybarColorNeutral is a color key only, never a monitor class: the prefix
	// and "/" of a percent token.
	waybarColorNeutral = "neutral"

	// waybarLongWindow splits a subscription's quota windows into its two bar
	// values: anything shorter (5h session) is the short value, weekly and
	// longer (Grok's monthly cycle) the long one.
	waybarLongWindow int64 = 7 * 24 * 3600
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
	model.PaceBelow:    "#7dffb3",
	model.PaceAt:       "#ffcc66",
	model.PaceOver:     "#ff5555",
	waybarClassAuth:    "#ffcc66",
	waybarClassError:   "#ff5555",
	waybarColorNeutral: "#ffffff",
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
// A subscription's compact text shows quota left in its short and long windows
// ("C 82%/63%"), each value colored by that window's own pace. Its class,
// percentage and tooltip cover every timed window, so percentage is whichever
// has the least left. Optional filters: provider, label, kind (comma-separated,
// case-insensitive).
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
		bars := barSpans(st.Spans, now)
		pct := remainingPct(st, now)
		cls := monitorClass(st, now)
		icon := icons[st.ID()]
		if icon == "" {
			icon = providerIcon(st.Provider)
		}
		vals := subscriptionValues(st, pct, now)
		text := compactText(st, icon, vals)
		detail := monitorDetail(st, now)
		pace, _ := barPace(st, now)
		if st.Kind == model.KindSubscription && pct != nil && *pct == 100 && st.OK && !st.NeedsAuth {
			// The number on the bar is remaining quota. 100% is untouched and
			// stays green even when pace is "at"/"over" or there is no window.
			pace = model.PaceBelow
			cls = model.PaceBelow
		}
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
			Windows:    windowsOf(vals),
			UpdatedAt:  st.UpdatedAt,
		}
		out.Monitors = append(out.Monitors, wm)
		texts = append(texts, compactMarkup(text, icon, vals, cls))
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

// barSpans picks the windows that drive the bar: every window with a known
// length (5h session and weekly caps alike), so the bar shows whichever runs
// out first. Unwindowed meters (extra credits) never drive it.
//
// A window whose reset time has passed has rolled over since the snapshot
// (last-good spans are reused for a while, and the cache is seeded from the
// DB on restart): it counts as untouched, with the next reset unknown, so a
// finished 5h session does not pin the bar near 0%.
func barSpans(spans []model.Span, now time.Time) []model.Span {
	out := make([]model.Span, 0, len(spans))
	for _, sp := range spans {
		if sp.WindowSeconds() <= 0 {
			continue
		}
		if sp.ResetAt > 0 && sp.ResetAt <= now.Unix() {
			sp.Used, sp.ResetAt = 0, 0
		}
		out = append(out, sp)
	}
	return out
}

// remainingPct is remaining quota in the bar windows (see barSpans): the
// minimum across the 5h and weekly windows.
func remainingPct(s model.Status, now time.Time) *int {
	if !s.OK {
		return nil
	}
	if left, ok := tightestRemaining(barSpans(s.Spans, now)); ok {
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

// authExpired reports a monitor that was connected but whose OAuth login can
// no longer be refreshed (refresh token expired or revoked). A monitor that
// was never connected reports "not connected …" instead.
func authExpired(s model.Status) bool {
	return s.NeedsAuth && !strings.HasPrefix(s.Error, "not connected")
}

func monitorClass(s model.Status, now time.Time) string {
	if authExpired(s) {
		// Usage stops updating until someone reconnects: show it red.
		return waybarClassError
	}
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
	tmp.Spans = barSpans(s.Spans, now)
	return tmp.WorstPace(now)
}

func colorize(text, class string) string {
	color, ok := waybarPaceColor[class]
	if !ok {
		return text
	}
	return fmt.Sprintf(`<span color="%s">%s</span>`, color, html.EscapeString(text))
}

// barValue is one percent on the bar and the class that colors it.
type barValue struct {
	pct   int
	class string
	group string
	win   waybarWindow // window, reset, linear left, delta (gadgets only)
}

// subscriptionValues returns the percents a healthy subscription shows: short
// window, then long. A monitor with one window has one value; windows of the
// same group (1w, 1w opus) show the least left and the worst pace among them.
// A subscription with no windows falls back to pct (its balance), and one with
// nothing to show returns nil.
func subscriptionValues(s model.Status, pct *int, now time.Time) []barValue {
	if s.Kind != model.KindSubscription || !s.OK || s.NeedsAuth {
		return nil
	}
	var short, long []model.Span
	for _, sp := range barSpans(s.Spans, now) {
		if sp.WindowSeconds() >= waybarLongWindow {
			long = append(long, sp)
		} else {
			short = append(short, sp)
		}
	}
	var out []barValue
	for i, group := range [][]model.Span{short, long} {
		if left, ok := tightestRemaining(group); ok {
			p := *clampPctPtr(int(math.Round(left)))
			pace, _ := model.Status{OK: true, Spans: group}.WorstPace(now)
			out = append(out, barValue{p, valueClass(p, pace), [2]string{"short", "long"}[i], groupDetail(group, p, now)})
		}
	}
	if len(out) == 0 && pct != nil {
		out = append(out, barValue{*pct, valueClass(*pct, ""), "balance", waybarWindow{}})
	}
	return out
}

func windowsOf(vals []barValue) []waybarWindow {
	if len(vals) == 0 {
		return nil
	}
	out := make([]waybarWindow, len(vals))
	for i, v := range vals {
		w := v.win
		w.Group, w.Left, w.Class = v.group, v.pct, v.class
		out[i] = w
	}
	return out
}

// groupDetail describes a window group (see waybarWindow): the tightest span
// sets the label, reset and linear left; Delta is the worst across the group.
func groupDetail(group []model.Span, left int, now time.Time) waybarWindow {
	tight := group[0]
	for _, sp := range group[1:] {
		if sp.Used > tight.Used {
			tight = sp
		}
	}
	w := waybarWindow{Window: windowLabel(tight.WindowSeconds())}
	if lin, ok := tight.LinearUsed(now); ok {
		w.ResetAt = tight.ResetAt
		w.LinearLeft = clampPctPtr(int(math.Round(100 - lin)))
	}
	for _, sp := range group {
		if lin, ok := sp.LinearUsed(now); ok {
			d := int(math.Round(lin - sp.Used))
			if w.Delta == nil || d < *w.Delta {
				w.Delta = &d
			}
		}
	}
	if w.Delta == nil && left == 100 {
		zero := 0
		w.Delta = &zero
	}
	return w
}

func windowLabel(secs int64) string {
	if secs >= 86400 {
		return fmt.Sprintf("%dd", secs/86400)
	}
	return fmt.Sprintf("%dh", secs/3600)
}

// valueClass is the pace ladder, except that 100% left is always green and 0%
// left always red. A value with no pace ("") stays uncolored.
func valueClass(pct int, pace string) string {
	switch pct {
	case 100:
		return model.PaceBelow
	case 0:
		return model.PaceOver
	}
	return pace
}

// compactMarkup is the Pango form of compactText. A percent token has a white
// prefix and "/" with each value in its own color; other tokens are one color.
func compactMarkup(text, icon string, vals []barValue, class string) string {
	if len(vals) == 0 {
		return colorize(text, class)
	}
	var b strings.Builder
	b.WriteString(colorize(icon+" ", waybarColorNeutral))
	for i, v := range vals {
		if i > 0 {
			b.WriteString(colorize("/", waybarColorNeutral))
		}
		b.WriteString(colorize(fmt.Sprintf("%d%%", v.pct), v.class))
	}
	return b.String()
}

func compactText(s model.Status, icon string, vals []barValue) string {
	switch {
	case authExpired(s):
		return icon + "!"
	case s.NeedsAuth:
		return icon + " ?"
	case !s.OK:
		return icon + " !"
	case s.Kind == model.KindSubscription:
		if len(vals) > 0 {
			parts := make([]string, len(vals))
			for i, v := range vals {
				parts[i] = fmt.Sprintf("%d%%", v.pct)
			}
			return icon + " " + strings.Join(parts, "/")
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
		if authExpired(s) {
			msg = "login expired — reconnect in the dashboard\n  " + msg
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
	if bars := barSpans(s.Spans, now); len(bars) > 0 {
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
		if pct := remainingPct(s, now); pct != nil {
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
