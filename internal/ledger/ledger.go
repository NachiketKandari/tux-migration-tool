// Package ledger records conversion state (PRD G8, §4.6): one status entry
// per plan unit (planned → generated → validated | failed → appended, plus
// blocked/skipped) so runs are resumable, and the source→target conversion
// map that answers "what happened to this function/query?" after the fact.
// State lives in conversion_logs/ledger/<service>.ledger.json.
package ledger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// UnitStatus is one plan unit's lifecycle state.
type UnitStatus string

const (
	StatusPlanned   UnitStatus = "planned"
	StatusGenerated UnitStatus = "generated"
	StatusValidated UnitStatus = "validated"
	StatusAppended  UnitStatus = "appended"
	StatusFailed    UnitStatus = "failed"
	// StatusBlocked is RESERVED — no pipeline stage sets it today (the
	// audit inverse-pattern: the summary used to advertise a count that
	// could never be non-zero). Kept for the later-version "blocked on
	// unresolved dependency" state; the summaries no longer print it.
	StatusBlocked UnitStatus = "blocked"
	StatusSkipped UnitStatus = "skipped"
	// StatusPlaceholder marks an unresolved construct rendered as a
	// compilable placeholder (PF-4.6): a terminal state alongside blocked —
	// the gap is known, greppable (tuxconv:TODO), and the build never breaks.
	StatusPlaceholder UnitStatus = "placeholder"
	// StatusDeviated marks a unit whose generated SQL drifted from the
	// source Tux SQL (PF-6): flag-only — the artifact landed and compiles,
	// the typed deviation is recorded for the reviewer; the run never fails.
	StatusDeviated UnitStatus = "deviated"
)

// Entry is one unit's durable state.
type Entry struct {
	ID       string     `json:"id"`
	Kind     string     `json:"kind"`
	Name     string     `json:"name"`
	Status   UnitStatus `json:"status"`
	Attempts int        `json:"attempts"`
	Error    string     `json:"error,omitempty"`
	// Targets is the §4.6 provenance record ("what did this unit produce?").
	// Its reader surface is the ledger JSON artifact itself — reviewed, not
	// queried in-process (engine-wiring audit Tier-2 note).
	Targets []string `json:"targets,omitempty"`
}

// MapEntry links one legacy section to its generated artifact (§4.6) — the
// reverse index of the provenance headers.
type MapEntry struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// Ledger is the durable conversion state for one service. Map is the §4.6
// reverse index — its reader surface is the ledger JSON artifact itself
// (reviewed after a run, not queried in-process; engine-wiring audit
// Tier-2 note: a query CLI is a later-version decision, not a silent drop).
type Ledger struct {
	Service string            `json:"service"`
	Units   map[string]*Entry `json:"units"`
	Map     []MapEntry        `json:"map"`

	path string
}

// Load reads <dir>/<service>.ledger.json, or returns a fresh ledger when
// absent — resume-safe by construction.
func Load(dir, service string) (*Ledger, error) {
	l := &Ledger{Service: service, Units: map[string]*Entry{}, path: filepath.Join(dir, service+".ledger.json")}
	data, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		return nil, fmt.Errorf("ledger: read %s: %w", l.path, err)
	}
	if err := json.Unmarshal(data, l); err != nil {
		return nil, fmt.Errorf("ledger: parse %s: %w", l.path, err)
	}
	if l.Units == nil {
		l.Units = map[string]*Entry{}
	}
	l.path = filepath.Join(dir, service+".ledger.json")
	return l, nil
}

// Fresh reports whether the ledger carries no unit records — a first run,
// not a resume.
func (l *Ledger) Fresh() bool { return len(l.Units) == 0 }

// Get returns the unit's entry, creating a planned one on first touch.
// Generation-change guard (audit 2026-09-16): plan unit IDs are positional,
// so a mapping rename reuses an ID for a different endpoint/method. When the
// recorded kind/name differs from the plan's, the entry is updated and any
// terminal status resets to planned — the unit regenerates instead of a
// resume silently mixing two generations in one tree.
func (l *Ledger) Get(id, kind, name string) *Entry {
	if e, ok := l.Units[id]; ok {
		// Entries created by Set carry no kind/name yet — backfill them.
		// Only a recorded kind/name that DIFFERS signals a mapping rename
		// (generation change) worth resetting to planned.
		if e.Kind == "" && e.Name == "" {
			e.Kind, e.Name = kind, name
			return e
		}
		if e.Kind != kind || e.Name != name {
			e.Kind = kind
			e.Name = name
			switch e.Status {
			case StatusAppended, StatusValidated, StatusGenerated,
				StatusSkipped, StatusFailed, StatusDeviated, StatusPlaceholder:
				e.Status = StatusPlanned
				e.Error = ""
				e.Targets = nil
			}
		}
		return e
	}
	e := &Entry{ID: id, Kind: kind, Name: name, Status: StatusPlanned}
	l.Units[id] = e
	return e
}

// Set transitions a unit and records the outcome.
func (l *Ledger) Set(id string, status UnitStatus, errMsg string, targets ...string) {
	e := l.Units[id]
	if e == nil {
		e = &Entry{ID: id}
		l.Units[id] = e
	}
	e.Status = status
	e.Error = errMsg
	if len(targets) > 0 {
		e.Targets = targets
	}
}

// AddMap records one source→target link (§4.6). Completeness: the convert
// pipeline adds one entry per generated artifact. Identical pairs dedup — a
// resume re-records the same links, and a mapping rename must not stack the
// old generation's links under the new one.
func (l *Ledger) AddMap(source, target string) {
	for _, m := range l.Map {
		if m.Source == source && m.Target == target {
			return
		}
	}
	l.Map = append(l.Map, MapEntry{Source: source, Target: target})
}

// Save persists the ledger (best-effort atomic: temp file + rename).
func (l *Ledger) Save() error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("ledger: marshal: %w", err)
	}
	tmp := l.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return fmt.Errorf("ledger: mkdir: %w", err)
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("ledger: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return fmt.Errorf("ledger: rename: %w", err)
	}
	return nil
}

// Counts summarizes the ledger for run summaries; placeholders report as a
// first-class count (PF-4.6) — the queryable inventory of what the tool does
// not know — and deviated units as the SQL fidelity count (PF-6.4).
func (l *Ledger) Counts() (appended, failed, blocked, skipped, placeholders, deviated int) {
	for _, e := range l.Units {
		switch e.Status {
		case StatusAppended, StatusValidated:
			appended++
		case StatusFailed:
			failed++
		case StatusBlocked:
			blocked++
		case StatusSkipped:
			skipped++
		case StatusPlaceholder:
			placeholders++
		case StatusDeviated:
			deviated++
		}
	}
	return
}
