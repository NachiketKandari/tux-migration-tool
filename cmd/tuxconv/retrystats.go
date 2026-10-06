package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"tux-migration-tool/internal/audit"
)

// runRetryStats compares the retry behaviour recorded in one or two audit
// run folders — the A/B read-out for the retry methodology experiment:
//
//	tuxconv retrystats conversion_logs/audit/<roll-run> conversion_logs/audit/<repair-run>
//
// Per unit it shows attempts, final outcome, and token spend; failed units
// list their last gate errors so the two methodologies can be judged on
// convergence, not just acceptance.
func runRetryStats(_ context.Context, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("usage: retrystats <audit-run-dir> [<audit-run-dir-B>]")
	}
	a, err := audit.CollectRun(args[0])
	if err != nil {
		return fmt.Errorf("retrystats: %w", err)
	}
	fmt.Printf("run A: %s\n", args[0])
	printRunSummary("A", a)
	if len(args) == 1 {
		printUnits(a, nil)
		printFailures("A", a)
		return nil
	}
	b, err := audit.CollectRun(args[1])
	if err != nil {
		return fmt.Errorf("retrystats: %w", err)
	}
	fmt.Printf("run B: %s\n", args[1])
	printRunSummary("B", b)
	printUnits(a, b)
	printFailures("A", a)
	printFailures("B", b)
	return nil
}

func printRunSummary(label string, r *audit.RunStats) {
	s := r.Summary()
	fmt.Printf("%s: mode=%s units=%d accepted=%d failed=%d first_try=%d attempts=%d tokens=%d (prompt=%d completion=%d)\n",
		label, r.Mode(), s.Units, s.Accepted, s.Failed, s.FirstTry, s.Attempts,
		s.TotalTokens, s.PromptTokens, s.CompletionTokens)
}

// printUnits renders the merged per-unit table. b == nil prints run A alone.
func printUnits(a, b *audit.RunStats) {
	keys := map[string]bool{}
	for k := range a.Units {
		keys[k] = true
	}
	if b != nil {
		for k := range b.Units {
			keys[k] = true
		}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	if b == nil {
		fmt.Fprintln(w, "unit\tattempts\toutcome\ttokens")
	} else {
		fmt.Fprintln(w, "unit\tA att\tA outcome\tA tokens\tB att\tB outcome\tB tokens")
	}
	for _, k := range sorted {
		if b == nil {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", k, attCell(a, k), outCell(a, k), tokCell(a, k))
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			k, attCell(a, k), outCell(a, k), tokCell(a, k),
			attCell(b, k), outCell(b, k), tokCell(b, k))
	}
	w.Flush()
}

// attCell / outCell / tokCell render one run's cell for a unit key, "-" when
// the unit is absent from that run.
func attCell(r *audit.RunStats, key string) string {
	if u := r.Units[key]; u != nil {
		return fmt.Sprintf("%d", u.Attempts)
	}
	return "-"
}

func outCell(r *audit.RunStats, key string) string {
	if u := r.Units[key]; u != nil {
		return u.Outcome
	}
	return "-"
}

func tokCell(r *audit.RunStats, key string) string {
	if u := r.Units[key]; u != nil {
		return fmt.Sprintf("%d", u.TotalTokens)
	}
	return "-"
}

// printFailures lists a run's failed units with their final gate errors —
// the convergence evidence the A/B needs (a repair run that fails the same
// way as the re-roll run is not an improvement).
func printFailures(label string, r *audit.RunStats) {
	var failed []string
	for k, u := range r.Units {
		if !u.Accepted {
			failed = append(failed, k)
		}
	}
	if len(failed) == 0 {
		return
	}
	sort.Strings(failed)
	fmt.Printf("\nfailed in %s:\n", label)
	for _, k := range failed {
		u := r.Units[k]
		msg := strings.Join(u.LastErrors, "; ")
		if len(msg) > 200 {
			msg = msg[:200] + "…"
		}
		fmt.Printf("  %s (%d attempts): %s\n", k, u.Attempts, msg)
	}
}
