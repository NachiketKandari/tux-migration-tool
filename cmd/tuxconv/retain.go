package main

import (
	"os"
	"path/filepath"
	"sort"
)

// maxRetainedRuns bounds local artifact growth: every invocation leaves a
// run-<id>.{jsonl,log} pair under conversion_logs/logs/ and a per-run dir
// under conversion_logs/audit/. Without a bound the tree grows forever on a
// dev machine. The ledger, state, and staged trees are NOT per-run and are
// never pruned (the ledger is resume state — deleting it would orphan
// staged output).
const maxRetainedRuns = 50

// pruneOldRuns deletes per-run artifacts beyond the newest keep in logDir
// (*.jsonl/*.log run files) and auditDir (per-run directories). Best-effort:
// unreadable dirs prune nothing, per-entry failures are skipped — rotation
// must never fail a conversion run. Returns the number of entries removed.
func pruneOldRuns(logDir, auditDir string, keep int) int {
	pruned := 0
	pruned += pruneFiles(logDir, keep)
	pruned += pruneDirs(auditDir, keep)
	return pruned
}

type entryAge struct {
	path  string
	mtime int64
}

func listAges(dir string, wantDir bool) []entryAge {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []entryAge
	for _, e := range entries {
		if e.IsDir() != wantDir {
			continue
		}
		p := filepath.Join(dir, e.Name())
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		out = append(out, entryAge{path: p, mtime: st.ModTime().UnixNano()})
	}
	// Oldest first; name tiebreak keeps the order deterministic when
	// several runs share a timestamp (same-second runs).
	sort.Slice(out, func(i, j int) bool {
		if out[i].mtime != out[j].mtime {
			return out[i].mtime < out[j].mtime
		}
		return out[i].path < out[j].path
	})
	return out
}

func pruneFiles(dir string, keep int) int {
	ages := listAges(dir, false)
	return dropOldest(ages, keep, os.Remove)
}

func pruneDirs(dir string, keep int) int {
	ages := listAges(dir, true)
	return dropOldest(ages, keep, os.RemoveAll)
}

func dropOldest(ages []entryAge, keep int, remove func(string) error) int {
	if keep < 0 {
		keep = 0
	}
	if len(ages) <= keep {
		return 0
	}
	pruned := 0
	for _, a := range ages[:len(ages)-keep] {
		if err := remove(a.path); err == nil {
			pruned++
		}
	}
	return pruned
}
