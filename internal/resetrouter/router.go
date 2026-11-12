package resetrouter

import (
	"context"
	"errors"
	"sort"
	"time"

	log "github.com/sirupsen/logrus"
)

// API is the subset of the management API the router uses.
type API interface {
	ListAccounts(ctx context.Context) ([]Account, error)
	SetPriority(ctx context.Context, id string, priority int) error
	ProbeUsage(ctx context.Context, account Account, now time.Time) (*Snapshot, error)
}

// Config controls ranking and probe pacing.
type Config struct {
	// Providers lists the providers to manage.
	Providers []string
	// BasePriority is the lowest priority the router assigns. Accounts whose
	// priority is outside [BasePriority, BasePriority+ManagedSpan) and not 0
	// are treated as pinned by hand and left alone.
	BasePriority int
	ManagedSpan  int
	// FreshFor is how old an account's quota data may be before it is probed.
	// Accounts serving traffic stay fresh from passive headers and are never probed.
	FreshFor time.Duration
	// MinProbeGap is the minimum time between two probes of one account.
	MinProbeGap time.Duration
	// MaxProbesPerCycle caps probes per cycle across all accounts.
	MaxProbesPerCycle int
	// BaseBackoff and MaxBackoff bound the exponential backoff after a failed probe.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// DryRun logs the planned priorities without writing them.
	DryRun bool
}

// DefaultConfig returns conservative defaults.
func DefaultConfig() Config {
	return Config{
		Providers:         []string{ProviderClaude, ProviderCodex},
		BasePriority:      1000,
		ManagedSpan:       1000,
		FreshFor:          45 * time.Minute,
		MinProbeGap:       15 * time.Minute,
		MaxProbesPerCycle: 2,
		BaseBackoff:       5 * time.Minute,
		MaxBackoff:        6 * time.Hour,
	}
}

// Router runs ranking cycles.
type Router struct {
	API    API
	Config Config
	State  *State
	Now    func() time.Time
}

// Change is one priority update planned or applied by a cycle.
type Change struct {
	Ranked
	Previous int
	Applied  bool
}

// Result summarises one cycle.
type Result struct {
	Ranked  map[string][]Ranked
	Changes []Change
	Probed  int
	Skipped []string
}

// Cycle lists accounts, probes the stalest ones that need it, ranks each
// provider and writes priorities that changed. When it cannot rank a provider
// safely it leaves that provider's priorities unchanged.
func (r *Router) Cycle(ctx context.Context) (Result, error) {
	result := Result{Ranked: make(map[string][]Ranked)}
	accounts, errList := r.API.ListAccounts(ctx)
	if errList != nil {
		return result, errList
	}
	now := r.now()

	keep := make(map[string]struct{}, len(accounts))
	managed := make(map[string][]Account)
	for _, account := range accounts {
		keep[account.ID] = struct{}{}
		if !r.manages(account) {
			continue
		}
		managed[account.Provider] = append(managed[account.Provider], account)
	}
	r.State.Prune(keep)

	result.Probed = r.probe(ctx, managed, now)
	now = r.now()

	var errApply error
	for _, provider := range r.Config.Providers {
		list := managed[provider]
		if len(list) == 0 {
			continue
		}
		if len(list) > r.Config.ManagedSpan {
			// Priorities would spill out of the managed range and the
			// spilled accounts would read as pinned next cycle.
			log.WithFields(log.Fields{"provider": provider, "accounts": len(list), "managed_span": r.Config.ManagedSpan}).
				Error("reset-router: more accounts than the managed priority range; raise --managed-span")
			result.Skipped = append(result.Skipped, provider)
			continue
		}
		quota := make(map[string]Snapshot, len(list))
		for _, account := range list {
			best := newer(account.Passive, r.State.account(account.ID).Snapshot)
			if best == nil {
				continue
			}
			view, _ := best.At(now)
			quota[account.ID] = view
		}
		if len(quota) == 0 {
			log.WithField("provider", provider).Info("reset-router: no quota data yet; leaving priorities unchanged")
			result.Skipped = append(result.Skipped, provider)
			continue
		}
		ranked := Rank(list, quota, now, r.Config.BasePriority)
		result.Ranked[provider] = ranked
		for _, entry := range ranked {
			if entry.Desired == entry.Account.Priority {
				continue
			}
			change := Change{Ranked: entry, Previous: entry.Account.Priority}
			if !r.Config.DryRun {
				if errSet := r.API.SetPriority(ctx, entry.Account.ID, entry.Desired); errSet != nil {
					errApply = errors.Join(errApply, errSet)
					log.WithError(errSet).WithField("credential", entry.Account.Name).Warn("reset-router: set priority failed")
					continue
				}
				change.Applied = true
			}
			result.Changes = append(result.Changes, change)
		}
	}
	return result, errApply
}

// manages reports whether the router owns this account's priority.
func (r *Router) manages(account Account) bool {
	if account.Disabled || account.RuntimeOnly || account.AuthIndex == "" {
		return false
	}
	enabled := false
	for _, provider := range r.Config.Providers {
		if provider == account.Provider {
			enabled = true
			break
		}
	}
	if !enabled {
		return false
	}
	if account.Priority == 0 {
		return true
	}
	return account.Priority >= r.Config.BasePriority && account.Priority < r.Config.BasePriority+r.Config.ManagedSpan
}

// probe refreshes the accounts whose data is missing or stale, stalest first,
// up to MaxProbesPerCycle.
func (r *Router) probe(ctx context.Context, managed map[string][]Account, now time.Time) int {
	type candidate struct {
		account  Account
		observed time.Time
	}
	var due []candidate
	for _, provider := range r.Config.Providers {
		for _, account := range managed[provider] {
			state := r.State.account(account.ID)
			best := newer(account.Passive, state.Snapshot)
			if !r.needsProbe(account, best, state, now) {
				continue
			}
			var observed time.Time
			if best != nil {
				observed = best.ObservedAt
			}
			due = append(due, candidate{account: account, observed: observed})
		}
	}
	sort.SliceStable(due, func(i, j int) bool {
		if !due[i].observed.Equal(due[j].observed) {
			return due[i].observed.Before(due[j].observed)
		}
		return due[i].account.Name < due[j].account.Name
	})
	if len(due) > r.Config.MaxProbesPerCycle {
		due = due[:r.Config.MaxProbesPerCycle]
	}
	for _, item := range due {
		if ctx.Err() != nil {
			break
		}
		r.probeOne(ctx, item.account)
	}
	return len(due)
}

func (r *Router) needsProbe(account Account, best *Snapshot, state *ProbeState, now time.Time) bool {
	if now.Before(state.NotBefore) {
		return false
	}
	if !state.LastAttempt.IsZero() && now.Sub(state.LastAttempt) < r.Config.MinProbeGap {
		return false
	}
	if best == nil {
		return true
	}
	view, stale := best.At(now)
	if stale {
		return true
	}
	// Usage only rises until a reset, so an exhausted window with a known
	// future reset cannot change before then; At reports it stale once it passes.
	for _, window := range []*Window{view.FiveHour, view.Weekly} {
		if window.Exhausted() && window.ResetAt.After(now) {
			return false
		}
	}
	return now.Sub(best.ObservedAt) >= jitter(r.Config.FreshFor, account.ID)
}

func (r *Router) probeOne(ctx context.Context, account Account) {
	now := r.now()
	state := r.State.account(account.ID)
	state.LastAttempt = now
	snapshot, errProbe := r.API.ProbeUsage(ctx, account, now)
	fields := log.Fields{"credential": account.Name, "provider": account.Provider}
	if errProbe != nil {
		state.Failures++
		delay := r.Config.BaseBackoff << min(state.Failures-1, 16)
		if delay <= 0 || delay > r.Config.MaxBackoff {
			delay = r.Config.MaxBackoff
		}
		delay = jitter(delay, account.ID)
		var probeErr *ProbeError
		if errors.As(errProbe, &probeErr) && probeErr.RetryAfter > delay {
			delay = probeErr.RetryAfter
		}
		state.NotBefore = now.Add(delay)
		fields["failures"] = state.Failures
		fields["retry_in"] = delay.Round(time.Second).String()
		log.WithError(errProbe).WithFields(fields).Warn("reset-router: usage probe failed")
		return
	}
	state.Failures = 0
	state.NotBefore = time.Time{}
	state.LastSuccess = now
	state.Snapshot = snapshot
	log.WithFields(fields).Debug("reset-router: usage probed")
}

func (r *Router) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}
