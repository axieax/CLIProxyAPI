package resetrouter

import (
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"time"

	"github.com/MoeForge/moego/ojson"
)

// ProbeState tracks active usage probes for one account so restarts and
// failures never turn into a burst of upstream calls.
type ProbeState struct {
	LastAttempt time.Time
	LastSuccess time.Time
	Failures    int
	NotBefore   time.Time
	Snapshot    *Snapshot
}

// State is the router's persisted memory, keyed by credential ID.
type State struct {
	Accounts map[string]*ProbeState
}

// NewState returns an empty state.
func NewState() *State {
	return &State{Accounts: make(map[string]*ProbeState)}
}

func (s *State) account(id string) *ProbeState {
	entry, ok := s.Accounts[id]
	if !ok {
		entry = &ProbeState{}
		s.Accounts[id] = entry
	}
	return entry
}

// Prune drops entries for credentials that no longer exist.
func (s *State) Prune(keep map[string]struct{}) {
	for id := range s.Accounts {
		if _, ok := keep[id]; !ok {
			delete(s.Accounts, id)
		}
	}
}

// LoadState reads the state file. A missing file yields an empty state.
func LoadState(path string) (*State, error) {
	state := NewState()
	data, errRead := os.ReadFile(path)
	if errors.Is(errRead, os.ErrNotExist) {
		return state, nil
	}
	if errRead != nil {
		return nil, fmt.Errorf("read state file: %w", errRead)
	}
	root := ojson.ParseBytes(data)
	if !root.IsObject() {
		return nil, fmt.Errorf("state file %s is not a JSON object", path)
	}
	root.Get("accounts").ForEach(func(key, value ojson.Result) bool {
		entry := &ProbeState{
			LastAttempt: unixTime(value.Get("last_attempt")),
			LastSuccess: unixTime(value.Get("last_success")),
			Failures:    int(value.Get("failures").Int()),
			NotBefore:   unixTime(value.Get("not_before")),
		}
		if snap := value.Get("snapshot"); snap.IsObject() {
			snapshot := &Snapshot{ObservedAt: unixTime(snap.Get("observed_at")), Source: SourceProbe}
			snapshot.FiveHour = readWindow(snap.Get("five_hour"))
			snapshot.Weekly = readWindow(snap.Get("weekly"))
			if !snapshot.Empty() && !snapshot.ObservedAt.IsZero() {
				entry.Snapshot = snapshot
			}
		}
		state.Accounts[key.String()] = entry
		return true
	})
	return state, nil
}

// Save writes the state atomically.
func (s *State) Save(path string) error {
	doc := ojson.NewDocString(`{"version":1,"accounts":{}}`)
	for id, entry := range s.Accounts {
		base := "accounts." + ojson.Escape(id)
		fields := map[string]any{
			"last_attempt": unixSeconds(entry.LastAttempt),
			"last_success": unixSeconds(entry.LastSuccess),
			"failures":     entry.Failures,
			"not_before":   unixSeconds(entry.NotBefore),
		}
		for key, value := range fields {
			if errSet := doc.Set(base+"."+key, value); errSet != nil {
				return errSet
			}
		}
		if entry.Snapshot != nil && !entry.Snapshot.Empty() {
			if errSet := doc.Set(base+".snapshot.observed_at", unixSeconds(entry.Snapshot.ObservedAt)); errSet != nil {
				return errSet
			}
			for key, window := range map[string]*Window{"five_hour": entry.Snapshot.FiveHour, "weekly": entry.Snapshot.Weekly} {
				if window == nil {
					continue
				}
				if errSet := doc.Set(base+".snapshot."+key+".used_percent", window.UsedPercent); errSet != nil {
					return errSet
				}
				if errSet := doc.Set(base+".snapshot."+key+".reset_at", unixSeconds(window.ResetAt)); errSet != nil {
					return errSet
				}
			}
		}
	}
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		return fmt.Errorf("create state directory: %w", errMkdir)
	}
	tmp, errCreate := os.CreateTemp(filepath.Dir(path), ".reset-router-state-*")
	if errCreate != nil {
		return fmt.Errorf("create temporary state file: %w", errCreate)
	}
	tmpName := tmp.Name()
	_, errWrite := tmp.Write(doc.Bytes())
	errClose := tmp.Close()
	if errWrite != nil || errClose != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write state file: %w", errors.Join(errWrite, errClose))
	}
	if errRename := os.Rename(tmpName, path); errRename != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace state file: %w", errRename)
	}
	return nil
}

func readWindow(node ojson.Result) *Window {
	if !node.IsObject() {
		return nil
	}
	return &Window{UsedPercent: clampPercent(node.Get("used_percent").Float()), ResetAt: unixTime(node.Get("reset_at"))}
}

func unixSeconds(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func unixTime(node ojson.Result) time.Time {
	if seconds := node.Int(); seconds > 0 {
		return time.Unix(seconds, 0).UTC()
	}
	return time.Time{}
}

// jitter spreads a duration by 0-20% using a stable per-account hash, so
// accounts do not all come due at the same moment.
func jitter(d time.Duration, id string) time.Duration {
	if d <= 0 {
		return d
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(id))
	return d + d*time.Duration(hash.Sum32()%21)/100
}
