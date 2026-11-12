package resetrouter

import (
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func TestParsePassiveSignalsClaude(t *testing.T) {
	signals := map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.25",
		"Anthropic-Ratelimit-Unified-5h-Reset":       "1794578400", // 2026-11-13T14:00:00Z
		"Anthropic-Ratelimit-Unified-7d-Utilization": "0.69",
		"Anthropic-Ratelimit-Unified-7d-Reset":       "1794873600", // 2026-11-17T00:00:00Z
		"Anthropic-Ratelimit-Unified-7d-Status":      "allowed",
	}
	snap := ParsePassiveSignals(ProviderClaude, signals, testNow)
	if snap == nil || snap.FiveHour == nil || snap.Weekly == nil {
		t.Fatalf("expected both windows, got %+v", snap)
	}
	if snap.FiveHour.UsedPercent != 25 || snap.Weekly.UsedPercent != 69 {
		t.Fatalf("used percent = %v / %v", snap.FiveHour.UsedPercent, snap.Weekly.UsedPercent)
	}
	if got := snap.Weekly.ResetAt.Unix(); got != 1794873600 {
		t.Fatalf("weekly reset = %d", got)
	}
}

func TestParsePassiveSignalsClaudeRejectedIsExhausted(t *testing.T) {
	snap := ParsePassiveSignals(ProviderClaude, map[string]string{
		"anthropic-ratelimit-unified-5h-status":      "rejected",
		"anthropic-ratelimit-unified-5h-utilization": "0.97",
	}, testNow)
	if snap == nil || !snap.FiveHour.Exhausted() {
		t.Fatalf("rejected 5h window should be exhausted: %+v", snap)
	}
}

func TestParsePassiveSignalsCodex(t *testing.T) {
	snap := ParsePassiveSignals(ProviderCodex, map[string]string{
		"X-Codex-Primary-Used-Percent":           "40",
		"X-Codex-Primary-Window-Minutes":         "300",
		"X-Codex-Primary-Reset-After-Seconds":    "600",
		"X-Codex-Secondary-Used-Percent":         "12",
		"X-Codex-Secondary-Window-Minutes":       "10080",
		"X-Codex-Secondary-Reset-At":             "1794873600",
		"X-Codex-Bengalfox-Primary-Used-Percent": "99",
	}, testNow)
	if snap == nil || snap.FiveHour == nil || snap.Weekly == nil {
		t.Fatalf("expected both windows, got %+v", snap)
	}
	if snap.FiveHour.UsedPercent != 40 || !snap.FiveHour.ResetAt.Equal(testNow.Add(10*time.Minute)) {
		t.Fatalf("5h window = %+v", snap.FiveHour)
	}
	if snap.Weekly.UsedPercent != 12 || snap.Weekly.ResetAt.Unix() != 1794873600 {
		t.Fatalf("weekly window = %+v", snap.Weekly)
	}
}

func TestParsePassiveSignalsCodexWeeklyPrimary(t *testing.T) {
	// Some plans only have a weekly window, reported as primary.
	snap := ParsePassiveSignals(ProviderCodex, map[string]string{
		"X-Codex-Primary-Used-Percent":   "5",
		"X-Codex-Primary-Window-Minutes": "10080",
		"X-Codex-Primary-Reset-At":       "1794873600",
	}, testNow)
	if snap == nil || snap.FiveHour != nil || snap.Weekly == nil {
		t.Fatalf("expected only a weekly window, got %+v", snap)
	}
}

func TestParsePassiveSignalsIgnoresEmpty(t *testing.T) {
	if snap := ParsePassiveSignals(ProviderCodex, map[string]string{"X-Codex-Plan-Type": "pro"}, testNow); snap != nil {
		t.Fatalf("expected nil, got %+v", snap)
	}
	if snap := ParsePassiveSignals(ProviderClaude, map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.1"}, time.Time{}); snap != nil {
		t.Fatalf("expected nil without observation time, got %+v", snap)
	}
}

func TestParseClaudeUsage(t *testing.T) {
	body := []byte(`{
		"five_hour": {"utilization": 12.5, "resets_at": "2026-11-13T14:00:00.123+00:00"},
		"seven_day": {"utilization": 100, "resets_at": "2026-11-17T00:00:00Z"},
		"seven_day_opus": {"utilization": 3, "resets_at": "2026-11-17T00:00:00Z"},
		"extra_usage": {"is_enabled": false}
	}`)
	snap := ParseClaudeUsage(body, testNow)
	if snap == nil || snap.Source != SourceProbe {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.FiveHour.UsedPercent != 12.5 || snap.FiveHour.ResetAt.Unix() != 1794578400 {
		t.Fatalf("5h = %+v", snap.FiveHour)
	}
	if !snap.Weekly.Exhausted() {
		t.Fatalf("weekly should be exhausted: %+v", snap.Weekly)
	}
}

func TestParseClaudeUsageNullWindow(t *testing.T) {
	snap := ParseClaudeUsage([]byte(`{"five_hour": null, "seven_day": {"utilization": 4, "resets_at": null}}`), testNow)
	if snap == nil || snap.FiveHour != nil || snap.Weekly == nil || !snap.Weekly.ResetAt.IsZero() {
		t.Fatalf("snapshot = %+v", snap)
	}
	if ParseClaudeUsage([]byte(`{"error":"x"}`), testNow) != nil {
		t.Fatal("expected nil for a response without windows")
	}
}

func TestParseCodexUsage(t *testing.T) {
	body := []byte(`{
		"plan_type": "pro",
		"rate_limit": {
			"allowed": true,
			"limit_reached": false,
			"primary_window": {"used_percent": 30, "limit_window_seconds": 18000, "reset_after_seconds": 3600, "reset_at": 1794578400},
			"secondary_window": {"used_percent": 55, "limit_window_seconds": 604800, "reset_at": 1794873600}
		},
		"code_review_rate_limit": {"primary_window": {"used_percent": 100, "limit_window_seconds": 604800, "reset_at": 1}}
	}`)
	snap := ParseCodexUsage(body, testNow)
	if snap == nil || snap.FiveHour.UsedPercent != 30 || snap.Weekly.UsedPercent != 55 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.FiveHour.ResetAt.Unix() != 1794578400 || snap.Weekly.ResetAt.Unix() != 1794873600 {
		t.Fatalf("resets = %v / %v", snap.FiveHour.ResetAt, snap.Weekly.ResetAt)
	}
}

func TestParseCodexUsageLimitReached(t *testing.T) {
	body := []byte(`{"rate_limit": {"limit_reached": true,
		"primary_window": {"used_percent": 80, "limit_window_seconds": 18000, "reset_after_seconds": 60}}}`)
	snap := ParseCodexUsage(body, testNow)
	if snap == nil || !snap.FiveHour.Exhausted() {
		t.Fatalf("limit_reached should exhaust the window: %+v", snap)
	}
}

func TestSnapshotAtRollsPastResets(t *testing.T) {
	snap := &Snapshot{
		ObservedAt: testNow.Add(-6 * time.Hour),
		FiveHour:   &Window{UsedPercent: 100, ResetAt: testNow.Add(-time.Minute)},
		Weekly:     &Window{UsedPercent: 50, ResetAt: testNow.Add(48 * time.Hour)},
	}
	view, stale := snap.At(testNow)
	if !stale {
		t.Fatal("a passed reset should mark the snapshot stale")
	}
	if view.FiveHour.Exhausted() || !view.FiveHour.ResetAt.IsZero() {
		t.Fatalf("passed 5h window should read as unused with unknown reset: %+v", view.FiveHour)
	}
	if view.Weekly.UsedPercent != 50 {
		t.Fatalf("weekly window should be unchanged: %+v", view.Weekly)
	}
	if snap.FiveHour.UsedPercent != 100 {
		t.Fatal("At must not mutate the stored snapshot")
	}
}

func TestNewerPrefersLatestNonEmpty(t *testing.T) {
	old := &Snapshot{ObservedAt: testNow.Add(-time.Hour), Weekly: &Window{}}
	recent := &Snapshot{ObservedAt: testNow, Weekly: &Window{}}
	if newer(old, recent) != recent || newer(recent, old) != recent {
		t.Fatal("newer should pick the latest snapshot")
	}
	if newer(&Snapshot{ObservedAt: testNow}, old) != old {
		t.Fatal("an empty snapshot should never win")
	}
	if newer(nil, nil) != nil {
		t.Fatal("expected nil")
	}
}
