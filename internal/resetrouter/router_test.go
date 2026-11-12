package resetrouter

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type fakeAPI struct {
	accounts   []Account
	listErr    error
	probes     map[string]*Snapshot
	probeErr   map[string]error
	probed     []string
	priorities map[string]int
	setErr     error
}

func (f *fakeAPI) ListAccounts(context.Context) ([]Account, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]Account, len(f.accounts))
	for index, account := range f.accounts {
		if priority, ok := f.priorities[account.ID]; ok {
			account.Priority = priority
		}
		out[index] = account
	}
	return out, nil
}

func (f *fakeAPI) SetPriority(_ context.Context, id string, priority int) error {
	if f.setErr != nil {
		return f.setErr
	}
	if f.priorities == nil {
		f.priorities = make(map[string]int)
	}
	f.priorities[id] = priority
	return nil
}

func (f *fakeAPI) ProbeUsage(_ context.Context, account Account, _ time.Time) (*Snapshot, error) {
	f.probed = append(f.probed, account.ID)
	if err := f.probeErr[account.ID]; err != nil {
		return nil, err
	}
	return f.probes[account.ID], nil
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func newRouter(api *fakeAPI, c *clock) *Router {
	return &Router{API: api, Config: DefaultConfig(), State: NewState(), Now: c.Now}
}

func claudeAccount(id string) Account {
	return Account{ID: id, Name: id, AuthIndex: "idx-" + id, Provider: ProviderClaude}
}

func fresh(weeklyIn time.Duration, at time.Time) *Snapshot {
	return &Snapshot{ObservedAt: at, Source: SourcePassive, Weekly: &Window{UsedPercent: 20, ResetAt: at.Add(weeklyIn)}}
}

func TestCycleAssignsPrioritiesAndUsesPassiveData(t *testing.T) {
	c := &clock{now: testNow}
	a, b := claudeAccount("a"), claudeAccount("b")
	a.Passive = fresh(96*time.Hour, testNow)
	b.Passive = fresh(5*time.Hour, testNow)
	api := &fakeAPI{accounts: []Account{a, b}}
	router := newRouter(api, c)

	result, err := router.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(api.probed) != 0 {
		t.Fatalf("fresh passive data must not trigger probes, probed %v", api.probed)
	}
	if api.priorities["b"] != 1001 || api.priorities["a"] != 1000 {
		t.Fatalf("priorities = %v", api.priorities)
	}
	if len(result.Changes) != 2 {
		t.Fatalf("changes = %d", len(result.Changes))
	}

	// A second cycle with the same data writes nothing.
	result, err = router.Cycle(context.Background())
	if err != nil || len(result.Changes) != 0 {
		t.Fatalf("expected no changes, got %d (err %v)", len(result.Changes), err)
	}
}

func TestCycleLeavesPinnedAndDisabledAccountsAlone(t *testing.T) {
	c := &clock{now: testNow}
	pinned := claudeAccount("pinned")
	pinned.Priority = 5000
	pinned.Passive = fresh(time.Hour, testNow)
	low := claudeAccount("low")
	low.Priority = 7
	disabled := claudeAccount("disabled")
	disabled.Disabled = true
	managed := claudeAccount("managed")
	managed.Passive = fresh(2*time.Hour, testNow)
	api := &fakeAPI{accounts: []Account{pinned, low, disabled, managed}}
	router := newRouter(api, c)

	if _, err := router.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.priorities) != 1 || api.priorities["managed"] != 1000 {
		t.Fatalf("only the managed account should change, got %v", api.priorities)
	}
}

func TestCycleSkipsProviderWithoutAnyData(t *testing.T) {
	c := &clock{now: testNow}
	api := &fakeAPI{
		accounts: []Account{claudeAccount("a"), claudeAccount("b")},
		probeErr: map[string]error{"a": errors.New("down"), "b": errors.New("down")},
	}
	router := newRouter(api, c)
	result, err := router.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(api.priorities) != 0 || len(result.Skipped) != 1 {
		t.Fatalf("provider without data must keep its order: priorities %v skipped %v", api.priorities, result.Skipped)
	}
}

func TestCycleRefusesToOverflowManagedRange(t *testing.T) {
	var accounts []Account
	for _, id := range []string{"a", "b", "c"} {
		account := claudeAccount(id)
		account.Passive = fresh(time.Hour, testNow)
		accounts = append(accounts, account)
	}
	api := &fakeAPI{accounts: accounts}
	router := newRouter(api, &clock{now: testNow})
	router.Config.ManagedSpan = 2
	result, err := router.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(api.priorities) != 0 || len(result.Skipped) != 1 {
		t.Fatalf("overflowing provider must be skipped: priorities %v skipped %v", api.priorities, result.Skipped)
	}
}

func TestCycleListFailureChangesNothing(t *testing.T) {
	api := &fakeAPI{listErr: errors.New("connection refused")}
	router := newRouter(api, &clock{now: testNow})
	if _, err := router.Cycle(context.Background()); err == nil {
		t.Fatal("expected the list error")
	}
	if len(api.priorities) != 0 || len(api.probed) != 0 {
		t.Fatal("nothing may happen when the list fails")
	}
}

func TestProbesAreCappedAndStalestFirst(t *testing.T) {
	c := &clock{now: testNow}
	var accounts []Account
	probes := map[string]*Snapshot{}
	for _, id := range []string{"a", "b", "c", "d"} {
		account := claudeAccount(id)
		accounts = append(accounts, account)
		probes[id] = &Snapshot{ObservedAt: testNow, Source: SourceProbe, Weekly: &Window{UsedPercent: 1, ResetAt: testNow.Add(time.Hour)}}
	}
	// Accounts with no data are stalest; c has data 2h old, so it comes after them.
	accounts[2].Passive = fresh(10*time.Hour, testNow.Add(-2*time.Hour))
	api := &fakeAPI{accounts: accounts, probes: probes}
	router := newRouter(api, c)

	if _, err := router.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.probed) != 2 || api.probed[0] != "a" || api.probed[1] != "b" {
		t.Fatalf("first cycle probed %v", api.probed)
	}
	c.now = c.now.Add(5 * time.Minute)
	api.probed = nil
	if _, err := router.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.probed) != 2 || api.probed[0] != "d" || api.probed[1] != "c" {
		t.Fatalf("second cycle probed %v", api.probed)
	}
}

func TestProbeRespectsFreshnessAndMinGap(t *testing.T) {
	c := &clock{now: testNow}
	account := claudeAccount("a")
	api := &fakeAPI{
		accounts: []Account{account},
		probes:   map[string]*Snapshot{"a": {ObservedAt: testNow, Weekly: &Window{UsedPercent: 1, ResetAt: testNow.Add(72 * time.Hour)}}},
	}
	router := newRouter(api, c)
	for step := 0; step < 9; step++ { // 40 minutes of 5-minute cycles
		if _, err := router.Cycle(context.Background()); err != nil {
			t.Fatal(err)
		}
		c.now = c.now.Add(5 * time.Minute)
	}
	if len(api.probed) != 1 {
		t.Fatalf("fresh data must not be re-probed within FreshFor, probed %d times", len(api.probed))
	}
	c.now = testNow.Add(router.Config.FreshFor*12/10 + time.Minute)
	if _, err := router.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.probed) != 2 {
		t.Fatalf("stale data should be probed again, probed %d times", len(api.probed))
	}
}

func TestExhaustedAccountIsNotProbedUntilReset(t *testing.T) {
	c := &clock{now: testNow}
	account := claudeAccount("a")
	resetAt := testNow.Add(3 * time.Hour)
	account.Passive = &Snapshot{ObservedAt: testNow.Add(-2 * time.Hour), Weekly: &Window{UsedPercent: 100, ResetAt: resetAt}}
	api := &fakeAPI{accounts: []Account{account}, probes: map[string]*Snapshot{"a": fresh(time.Hour, testNow)}}
	router := newRouter(api, c)
	if _, err := router.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.probed) != 0 {
		t.Fatal("an exhausted window with a future reset must not be probed")
	}
	c.now = resetAt.Add(time.Second)
	if _, err := router.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.probed) != 1 {
		t.Fatal("the account should be probed once its reset passes")
	}
}

func TestProbeFailureBacksOffAndHonoursRetryAfter(t *testing.T) {
	c := &clock{now: testNow}
	api := &fakeAPI{
		accounts: []Account{claudeAccount("a")},
		probeErr: map[string]error{"a": &ProbeError{StatusCode: 429, RetryAfter: 2 * time.Hour}},
	}
	router := newRouter(api, c)
	if _, err := router.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := router.State.Accounts["a"]
	if state.Failures != 1 || !state.NotBefore.Equal(testNow.Add(2*time.Hour)) {
		t.Fatalf("state = %+v", state)
	}
	c.now = testNow.Add(time.Hour)
	if _, err := router.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.probed) != 1 {
		t.Fatal("must not probe before Retry-After")
	}

	// Without Retry-After the backoff grows exponentially up to the cap.
	api.probeErr["a"] = errors.New("boom")
	router.State.Accounts["a"] = &ProbeState{}
	var previous time.Duration
	for attempt := 1; attempt <= 10; attempt++ {
		router.probeOne(context.Background(), claudeAccount("a"))
		delay := router.State.Accounts["a"].NotBefore.Sub(c.now)
		if delay < previous || delay > jitter(router.Config.MaxBackoff, "a") {
			t.Fatalf("attempt %d delay %v (previous %v)", attempt, delay, previous)
		}
		previous = delay
	}
	if previous < router.Config.MaxBackoff {
		t.Fatalf("backoff should reach the cap, got %v", previous)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	c := &clock{now: testNow}
	api := &fakeAPI{
		accounts: []Account{claudeAccount("claude-a@example.com.json")},
		probes: map[string]*Snapshot{"claude-a@example.com.json": {
			ObservedAt: testNow, Source: SourceProbe,
			FiveHour: &Window{UsedPercent: 12.5, ResetAt: testNow.Add(time.Hour)},
			Weekly:   &Window{UsedPercent: 40, ResetAt: testNow.Add(50 * time.Hour)},
		}},
	}
	router := newRouter(api, c)
	if _, err := router.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := router.State.Save(path); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	entry := loaded.Accounts["claude-a@example.com.json"]
	if entry == nil || entry.Snapshot == nil || entry.Snapshot.FiveHour.UsedPercent != 12.5 ||
		!entry.Snapshot.Weekly.ResetAt.Equal(testNow.Add(50*time.Hour)) || !entry.LastAttempt.Equal(testNow) {
		t.Fatalf("loaded = %+v", entry)
	}

	// A restarted router reuses the stored snapshot instead of probing again.
	restarted := &Router{API: api, Config: DefaultConfig(), State: loaded, Now: c.Now}
	api.probed = nil
	c.now = testNow.Add(10 * time.Minute)
	if _, err := restarted.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.probed) != 0 {
		t.Fatalf("restart must not re-probe fresh accounts, probed %v", api.probed)
	}
}

func TestLoadStateMissingFile(t *testing.T) {
	state, err := LoadState(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || len(state.Accounts) != 0 {
		t.Fatalf("state %v err %v", state, err)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	a := claudeAccount("a")
	a.Passive = fresh(time.Hour, testNow)
	api := &fakeAPI{accounts: []Account{a}}
	router := newRouter(api, &clock{now: testNow})
	router.Config.DryRun = true
	result, err := router.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(api.priorities) != 0 || len(result.Changes) != 1 || result.Changes[0].Applied {
		t.Fatalf("dry run wrote %v, changes %+v", api.priorities, result.Changes)
	}
}
