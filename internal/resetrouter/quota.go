// Package resetrouter ranks Claude and Codex OAuth credentials so CLIProxyAPI
// spends the quota that resets soonest first. It runs outside the proxy, reads
// quota through the v8 management API, and expresses its decision only through
// each credential's routing priority.
package resetrouter

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
)

// Provider names handled by the router.
const (
	ProviderClaude = "claude"
	ProviderCodex  = "codex"
)

// Snapshot sources.
const (
	SourcePassive = "passive"
	SourceProbe   = "probe"
)

const (
	fiveHourMaxSeconds = 6 * 60 * 60
	weeklyMinSeconds   = 6 * 24 * 60 * 60
)

// Window is one quota window. UsedPercent is 0-100; a zero ResetAt means the
// reset time is unknown.
type Window struct {
	UsedPercent float64
	ResetAt     time.Time
}

// Exhausted reports whether the window has no usable quota left.
func (w *Window) Exhausted() bool {
	return w != nil && w.UsedPercent >= 100
}

// Snapshot is the quota state of one credential at ObservedAt.
type Snapshot struct {
	ObservedAt time.Time
	Source     string
	FiveHour   *Window
	Weekly     *Window
}

// Empty reports whether the snapshot carries no usable window.
func (s *Snapshot) Empty() bool {
	return s == nil || (s.FiveHour == nil && s.Weekly == nil)
}

// At returns the snapshot as it applies at now. A window whose reset time has
// passed is reported as unused with an unknown reset, and stale is true so the
// caller can refresh it.
func (s *Snapshot) At(now time.Time) (view Snapshot, stale bool) {
	if s == nil {
		return Snapshot{}, true
	}
	view = Snapshot{ObservedAt: s.ObservedAt, Source: s.Source}
	roll := func(w *Window) *Window {
		if w == nil {
			return nil
		}
		copied := *w
		if !copied.ResetAt.IsZero() && !copied.ResetAt.After(now) {
			stale = true
			return &Window{}
		}
		return &copied
	}
	view.FiveHour = roll(s.FiveHour)
	view.Weekly = roll(s.Weekly)
	return view, stale
}

// newer returns whichever non-empty snapshot was observed last.
func newer(a, b *Snapshot) *Snapshot {
	switch {
	case a.Empty():
		if b.Empty() {
			return nil
		}
		return b
	case b.Empty():
		return a
	case b.ObservedAt.After(a.ObservedAt):
		return b
	default:
		return a
	}
}

// ParsePassiveSignals converts the quota headers CLIProxyAPI records from real
// upstream responses into a snapshot. It returns nil when no window is present.
func ParsePassiveSignals(provider string, signals map[string]string, observedAt time.Time) *Snapshot {
	if len(signals) == 0 || observedAt.IsZero() {
		return nil
	}
	lookup := make(map[string]string, len(signals))
	for key, value := range signals {
		lookup[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	snapshot := &Snapshot{ObservedAt: observedAt.UTC(), Source: SourcePassive}
	switch provider {
	case ProviderClaude:
		snapshot.FiveHour = claudeHeaderWindow(lookup, "5h")
		snapshot.Weekly = claudeHeaderWindow(lookup, "7d")
	case ProviderCodex:
		for _, prefix := range []string{"primary", "secondary"} {
			window, seconds := codexHeaderWindow(lookup, prefix, observedAt)
			if window == nil {
				continue
			}
			if seconds == 0 {
				// Codex reports the 5h window as primary and the weekly one as
				// secondary when the duration header is absent.
				seconds = map[string]int64{"primary": fiveHourMaxSeconds, "secondary": weeklyMinSeconds}[prefix]
			}
			assignWindow(snapshot, window, seconds)
		}
	default:
		return nil
	}
	if snapshot.Empty() {
		return nil
	}
	return snapshot
}

// claudeHeaderWindow reads anthropic-ratelimit-unified-<claim>-* headers.
// Utilization is a 0-1 fraction and reset is Unix seconds.
func claudeHeaderWindow(lookup map[string]string, claim string) *Window {
	prefix := "anthropic-ratelimit-unified-" + claim + "-"
	utilization, okUtil := parseFloat(lookup[prefix+"utilization"])
	status := strings.ToLower(lookup[prefix+"status"])
	if !okUtil && status == "" {
		return nil
	}
	window := &Window{UsedPercent: clampPercent(utilization * 100)}
	if status == "rejected" {
		window.UsedPercent = 100
	}
	if reset, okReset := parseFloat(lookup[prefix+"reset"]); okReset && reset > 0 {
		window.ResetAt = time.Unix(int64(reset), 0).UTC()
	}
	return window
}

// codexHeaderWindow reads x-codex-<prefix>-* headers. It returns the window
// and its duration in seconds (0 when unknown).
func codexHeaderWindow(lookup map[string]string, prefix string, observedAt time.Time) (*Window, int64) {
	base := "x-codex-" + prefix + "-"
	used, okUsed := parseFloat(lookup[base+"used-percent"])
	if !okUsed {
		return nil, 0
	}
	window := &Window{UsedPercent: clampPercent(used)}
	if resetAt, okReset := parseFloat(lookup[base+"reset-at"]); okReset && resetAt > 0 {
		window.ResetAt = time.Unix(int64(resetAt), 0).UTC()
	} else if after, okAfter := parseFloat(lookup[base+"reset-after-seconds"]); okAfter && after >= 0 {
		window.ResetAt = observedAt.UTC().Add(time.Duration(after) * time.Second)
	}
	var seconds int64
	if minutes, okMinutes := parseFloat(lookup[base+"window-minutes"]); okMinutes && minutes > 0 {
		seconds = int64(minutes * 60)
	}
	return window, seconds
}

// ParseClaudeUsage parses GET https://api.anthropic.com/api/oauth/usage.
// Utilization is a 0-100 percentage and resets_at is RFC 3339.
func ParseClaudeUsage(body []byte, observedAt time.Time) *Snapshot {
	root := ojson.ParseBytes(body)
	if !root.IsObject() {
		return nil
	}
	read := func(key string) *Window {
		node := root.Get(key)
		utilization := node.Get("utilization")
		if !node.IsObject() || utilization.Type != ojson.Number {
			return nil
		}
		window := &Window{UsedPercent: clampPercent(utilization.Float())}
		if resetAt, errParse := time.Parse(time.RFC3339Nano, strings.TrimSpace(node.Get("resets_at").String())); errParse == nil {
			window.ResetAt = resetAt.UTC()
		}
		return window
	}
	snapshot := &Snapshot{ObservedAt: observedAt.UTC(), Source: SourceProbe, FiveHour: read("five_hour"), Weekly: read("seven_day")}
	if snapshot.Empty() {
		return nil
	}
	return snapshot
}

// ParseCodexUsage parses GET https://chatgpt.com/backend-api/wham/usage.
func ParseCodexUsage(body []byte, observedAt time.Time) *Snapshot {
	rateLimit := ojson.ParseBytes(body).Get("rate_limit")
	if !rateLimit.IsObject() {
		return nil
	}
	snapshot := &Snapshot{ObservedAt: observedAt.UTC(), Source: SourceProbe}
	for _, key := range []string{"primary_window", "secondary_window"} {
		node := rateLimit.Get(key)
		used := node.Get("used_percent")
		seconds := node.Get("limit_window_seconds").Int()
		if !node.IsObject() || used.Type != ojson.Number || seconds <= 0 {
			continue
		}
		window := &Window{UsedPercent: clampPercent(used.Float())}
		if resetAt := node.Get("reset_at").Int(); resetAt > 0 {
			window.ResetAt = time.Unix(resetAt, 0).UTC()
		} else if after := node.Get("reset_after_seconds"); after.Type == ojson.Number && after.Int() >= 0 {
			window.ResetAt = observedAt.UTC().Add(time.Duration(after.Int()) * time.Second)
		}
		assignWindow(snapshot, window, seconds)
	}
	if limitReached := rateLimit.Get("limit_reached"); limitReached.IsBool() && limitReached.Bool() {
		// The account is blocked even if neither window reads 100%; mark the
		// shortest known window exhausted so it is not promoted.
		switch {
		case snapshot.FiveHour != nil:
			snapshot.FiveHour.UsedPercent = 100
		case snapshot.Weekly != nil:
			snapshot.Weekly.UsedPercent = 100
		}
	}
	if snapshot.Empty() {
		return nil
	}
	return snapshot
}

func assignWindow(snapshot *Snapshot, window *Window, seconds int64) {
	switch {
	case seconds <= fiveHourMaxSeconds:
		if snapshot.FiveHour == nil {
			snapshot.FiveHour = window
		}
	case seconds >= weeklyMinSeconds:
		if snapshot.Weekly == nil {
			snapshot.Weekly = window
		}
	}
}

func parseFloat(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	parsed, errParse := strconv.ParseFloat(value, 64)
	if errParse != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	return parsed, true
}

func clampPercent(value float64) float64 {
	if math.IsNaN(value) || value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}
