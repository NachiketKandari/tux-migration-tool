package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// exchangeFileRe matches the seam's per-attempt artifact names
// ("<kind>-<name>-attempt<N>.json"); every Exchange record lands under this
// convention, so it is the CollectRun filter (IR/plan/report artifacts in
// the same folder never match).
var exchangeFileRe = regexp.MustCompile(`-attempt\d+\.json$`)

// RunStats aggregates one audit run's Exchange trail for retry-methodology
// comparison (tuxconv retrystats). Units are keyed "<kind>/<name>"; chunk
// and composer passes appear as their own units (name#chunk1, name#composer).
type RunStats struct {
	Dir    string
	Repair bool // any record rode the repair strategy
	roll   bool // any record rode the re-roll strategy
	Units  map[string]*UnitStats
}

// UnitStats is one unit's attempt history across the run.
type UnitStats struct {
	Kind             string
	Name             string
	Attempts         int
	Accepted         bool
	FirstTry         bool // accepted on attempt 0
	Outcome          string
	LastErrors       []string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	lastAttempt      int
}

// Mode names the retry methodology the run's records carried.
func (r *RunStats) Mode() string {
	switch {
	case r.Repair && r.roll:
		return "mixed"
	case r.Repair:
		return "repair"
	default:
		return "re-roll"
	}
}

// Summary totals across units.
type Summary struct {
	Units            int
	Accepted         int
	Failed           int
	FirstTry         int
	Attempts         int
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// Summary aggregates the run's per-unit stats.
func (r *RunStats) Summary() Summary {
	var s Summary
	for _, u := range r.Units {
		s.Units++
		s.Attempts += u.Attempts
		s.PromptTokens += u.PromptTokens
		s.CompletionTokens += u.CompletionTokens
		s.TotalTokens += u.TotalTokens
		if u.Accepted {
			s.Accepted++
		} else {
			s.Failed++
		}
		if u.FirstTry {
			s.FirstTry++
		}
	}
	return s
}

// UnitKeys returns the unit keys in deterministic order.
func (r *RunStats) UnitKeys() []string {
	keys := make([]string, 0, len(r.Units))
	for k := range r.Units {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// CollectRun reads one audit run folder's Exchange artifacts. Unreadable or
// non-Exchange files are skipped — the audit folder also carries IR, plan,
// and report artifacts, and a stats tool must never fail on them.
func CollectRun(dir string) (*RunStats, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	r := &RunStats{Dir: dir, Units: map[string]*UnitStats{}}
	for _, de := range entries {
		if de.IsDir() || !exchangeFileRe.MatchString(de.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			return nil, err
		}
		var e Exchange
		if err := json.Unmarshal(data, &e); err != nil || (e.Kind == "" && e.Name == "") {
			continue
		}
		if e.RetryRepair {
			r.Repair = true
		} else {
			r.roll = true
		}
		key := e.Kind + "/" + e.Name
		u := r.Units[key]
		if u == nil {
			u = &UnitStats{Kind: e.Kind, Name: e.Name, lastAttempt: -1}
			r.Units[key] = u
		}
		u.Attempts++
		u.PromptTokens += e.PromptTokens
		u.CompletionTokens += e.CompletionTokens
		u.TotalTokens += e.TotalTokens
		if e.Outcome == "ok" {
			u.Accepted = true
			if e.Attempt == 0 {
				u.FirstTry = true
			}
		}
		if e.Attempt >= u.lastAttempt {
			u.lastAttempt = e.Attempt
			u.Outcome = e.Outcome
			u.LastErrors = e.Errors
		}
	}
	return r, nil
}
