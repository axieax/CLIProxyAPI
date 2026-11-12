package resetrouter

import (
	"sort"
	"time"
)

// Account is one credential as reported by GET /v8/management/credentials.
type Account struct {
	ID             string
	Name           string
	AuthIndex      string
	Provider       string
	Priority       int
	Disabled       bool
	RuntimeOnly    bool
	Unavailable    bool
	NextRetryAfter time.Time
	ChatGPTAccount string
	Passive        *Snapshot
}

// Tier orders accounts coarsely; lower tiers are preferred.
type Tier int

const (
	// TierWeeklyKnown accounts have quota now and a known weekly reset.
	TierWeeklyKnown Tier = iota
	// TierWeeklyUnknown accounts have quota now but no known weekly reset.
	TierWeeklyUnknown
	// TierBlocked accounts cannot serve now: an exhausted window or a proxy cooldown.
	TierBlocked
)

func (t Tier) String() string {
	switch t {
	case TierWeeklyKnown:
		return "weekly-known"
	case TierWeeklyUnknown:
		return "weekly-unknown"
	default:
		return "blocked"
	}
}

// Ranked is one account with its ranking inputs and assigned priority.
type Ranked struct {
	Account  Account
	Quota    Snapshot
	Tier     Tier
	Recovery time.Time
	Desired  int
}

// Rank orders accounts so the quota that resets soonest is spent first and
// assigns descending priorities starting at base+len(accounts)-1.
//
// Within TierWeeklyKnown the earliest weekly reset wins, compared to the
// minute so second-level jitter does not reorder accounts; the 5h reset breaks
// ties. Blocked accounts sort by when they recover. Name is the final tie-break
// so the order is deterministic.
func Rank(accounts []Account, quota map[string]Snapshot, now time.Time, base int) []Ranked {
	ranked := make([]Ranked, 0, len(accounts))
	for _, account := range accounts {
		view := quota[account.ID]
		entry := Ranked{Account: account, Quota: view}
		entry.Tier, entry.Recovery = classify(account, view, now)
		ranked = append(ranked, entry)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.Tier != b.Tier {
			return a.Tier < b.Tier
		}
		switch a.Tier {
		case TierWeeklyKnown:
			if c := compareTimes(a.Quota.Weekly.ResetAt.Truncate(time.Minute), b.Quota.Weekly.ResetAt.Truncate(time.Minute)); c != 0 {
				return c < 0
			}
			if c := compareTimes(resetOf(a.Quota.FiveHour), resetOf(b.Quota.FiveHour)); c != 0 {
				return c < 0
			}
		case TierWeeklyUnknown:
			if c := compareTimes(resetOf(a.Quota.FiveHour), resetOf(b.Quota.FiveHour)); c != 0 {
				return c < 0
			}
		case TierBlocked:
			if c := compareTimes(a.Recovery, b.Recovery); c != 0 {
				return c < 0
			}
		}
		return a.Account.Name < b.Account.Name
	})
	for index := range ranked {
		ranked[index].Desired = base + len(ranked) - 1 - index
	}
	return ranked
}

func classify(account Account, view Snapshot, now time.Time) (Tier, time.Time) {
	var recovery time.Time
	blocked := false
	if account.Unavailable {
		blocked = true
		recovery = laterOf(recovery, account.NextRetryAfter)
	}
	for _, window := range []*Window{view.FiveHour, view.Weekly} {
		if window.Exhausted() {
			blocked = true
			recovery = laterOf(recovery, window.ResetAt)
		}
	}
	if blocked {
		if !recovery.IsZero() && !recovery.After(now) {
			recovery = time.Time{}
		}
		return TierBlocked, recovery
	}
	if view.Weekly != nil && !view.Weekly.ResetAt.IsZero() {
		return TierWeeklyKnown, time.Time{}
	}
	return TierWeeklyUnknown, time.Time{}
}

func resetOf(window *Window) time.Time {
	if window == nil {
		return time.Time{}
	}
	return window.ResetAt
}

// compareTimes orders known times ascending and unknown (zero) times last.
func compareTimes(a, b time.Time) int {
	switch {
	case a.Equal(b):
		return 0
	case a.IsZero():
		return 1
	case b.IsZero():
		return -1
	case a.Before(b):
		return -1
	default:
		return 1
	}
}

func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
