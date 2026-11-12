package resetrouter

import (
	"testing"
	"time"
)

func weekly(used float64, in time.Duration) *Window {
	return &Window{UsedPercent: used, ResetAt: testNow.Add(in)}
}

func names(ranked []Ranked) []string {
	out := make([]string, len(ranked))
	for index, entry := range ranked {
		out[index] = entry.Account.Name
	}
	return out
}

func assertOrder(t *testing.T, ranked []Ranked, want ...string) {
	t.Helper()
	got := names(ranked)
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestRankSoonestWeeklyResetFirst(t *testing.T) {
	accounts := []Account{{ID: "a", Name: "a"}, {ID: "b", Name: "b"}, {ID: "c", Name: "c"}}
	quota := map[string]Snapshot{
		"a": {Weekly: weekly(10, 72*time.Hour)},
		"b": {Weekly: weekly(90, 2*time.Hour)},
		"c": {Weekly: weekly(50, 24*time.Hour)},
	}
	ranked := Rank(accounts, quota, testNow, 1000)
	assertOrder(t, ranked, "b", "c", "a")
	if ranked[0].Desired != 1002 || ranked[2].Desired != 1000 {
		t.Fatalf("priorities = %d..%d", ranked[0].Desired, ranked[2].Desired)
	}
}

func TestRankTiers(t *testing.T) {
	accounts := []Account{
		{ID: "exhausted-weekly", Name: "exhausted-weekly"},
		{ID: "unknown", Name: "unknown"},
		{ID: "cooldown", Name: "cooldown", Unavailable: true, NextRetryAfter: testNow.Add(time.Hour)},
		{ID: "five-hour-out", Name: "five-hour-out"},
		{ID: "known", Name: "known"},
	}
	quota := map[string]Snapshot{
		"exhausted-weekly": {Weekly: weekly(100, 3*time.Hour)},
		"cooldown":         {Weekly: weekly(10, time.Hour)},
		"five-hour-out":    {FiveHour: &Window{UsedPercent: 100, ResetAt: testNow.Add(30 * time.Minute)}, Weekly: weekly(10, time.Hour)},
		"known":            {Weekly: weekly(10, 100*time.Hour)},
	}
	ranked := Rank(accounts, quota, testNow, 0)
	// Blocked accounts sort by recovery: 30m, 1h, 3h.
	assertOrder(t, ranked, "known", "unknown", "five-hour-out", "cooldown", "exhausted-weekly")
	if ranked[1].Tier != TierWeeklyUnknown || ranked[2].Tier != TierBlocked {
		t.Fatalf("tiers = %v %v", ranked[1].Tier, ranked[2].Tier)
	}
}

func TestRankMinuteJitterFallsBackToFiveHourThenName(t *testing.T) {
	accounts := []Account{{ID: "z", Name: "z"}, {ID: "y", Name: "y"}, {ID: "x", Name: "x"}}
	quota := map[string]Snapshot{
		"z": {Weekly: weekly(10, 24*time.Hour+10*time.Second), FiveHour: &Window{ResetAt: testNow.Add(time.Hour)}},
		"y": {Weekly: weekly(10, 24*time.Hour+20*time.Second), FiveHour: &Window{ResetAt: testNow.Add(2 * time.Hour)}},
		"x": {Weekly: weekly(10, 24*time.Hour+30*time.Second)},
	}
	assertOrder(t, Rank(accounts, quota, testNow, 0), "z", "y", "x")
}

func TestRankIsDeterministic(t *testing.T) {
	accounts := []Account{{ID: "b", Name: "b"}, {ID: "a", Name: "a"}}
	first := names(Rank(accounts, nil, testNow, 0))
	accounts[0], accounts[1] = accounts[1], accounts[0]
	second := names(Rank(accounts, nil, testNow, 0))
	if first[0] != second[0] || first[0] != "a" {
		t.Fatalf("orders differ: %v vs %v", first, second)
	}
}
