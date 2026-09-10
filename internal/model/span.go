package model

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// How actual usage compares to a linear burn across the quota window.
const (
	PaceBelow = "below" // used slower than linear — under budget
	PaceAt    = "at"    // on the linear line
	PaceOver  = "over"  // used faster than linear — burning too fast
)

// PaceSlack is the ±percentage-point band around the linear line that still
// counts as "at" (avoids flicker when usage is a point or two off).
const PaceSlack = 5.0

var windowNameRe = regexp.MustCompile(`(?i)(\d+)\s*(mo|w|d|h|m|s)\b`)

var paceRank = map[string]int{
	PaceBelow: 1,
	PaceAt:    2,
	PaceOver:  3,
}

// ParseWindowDuration reads a length from a span name like "5h", "1w",
// "1w opus", or "5h Spark". "mo" is a 30-day month; "m" is minutes.
func ParseWindowDuration(name string) int64 {
	m := windowNameRe.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	switch strings.ToLower(m[2]) {
	case "mo":
		return n * 30 * 24 * 3600
	case "w":
		return n * 7 * 24 * 3600
	case "d":
		return n * 24 * 3600
	case "h":
		return n * 3600
	case "m":
		return n * 60
	case "s":
		return n
	}
	return 0
}

// WindowSeconds is the quota window length: Duration if set, else the name.
func (sp Span) WindowSeconds() int64 {
	if sp.Duration > 0 {
		return sp.Duration
	}
	return ParseWindowDuration(sp.Name)
}

// LinearUsed is the percent that would be consumed if spend were linear
// from (ResetAt − window) to ResetAt. ok is false when the window is unknown.
func (sp Span) LinearUsed(now time.Time) (float64, bool) {
	dur := sp.WindowSeconds()
	if dur <= 0 || sp.ResetAt <= 0 {
		return 0, false
	}
	elapsed := dur - (sp.ResetAt - now.Unix())
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed > dur {
		elapsed = dur
	}
	return 100 * float64(elapsed) / float64(dur), true
}

// Pace classifies Used against the linear line at now.
func (sp Span) Pace(now time.Time) (class string, expected float64, ok bool) {
	expected, ok = sp.LinearUsed(now)
	if !ok {
		return "", 0, false
	}
	d := sp.Used - expected
	switch {
	case d > PaceSlack:
		return PaceOver, expected, true
	case d < -PaceSlack:
		return PaceBelow, expected, true
	default:
		return PaceAt, expected, true
	}
}

// WorstPace is the most severe pace among spans that can be scored
// (over > at > below). ok is false when no span has a window.
func (s Status) WorstPace(now time.Time) (string, bool) {
	if !s.OK {
		return "", false
	}
	worst := ""
	for _, sp := range s.Spans {
		class, _, ok := sp.Pace(now)
		if !ok {
			continue
		}
		if paceRank[class] > paceRank[worst] {
			worst = class
		}
	}
	return worst, worst != ""
}
