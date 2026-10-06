package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tux-migration-tool/internal/telemetry"
)

const version = "0.1.0-tuxconv"

// Durable per-run artifact locations (architecture.md §4): conversion_logs/
// is the single root for everything the tool writes at runtime — the unified
// slog stream in conversion_logs/logs/, the per-run audit trail (with the
// per-file IR snapshots) in conversion_logs/audit/.
const (
	defaultLogDir = "conversion_logs/logs"
	auditDir      = "conversion_logs/audit"
)

func main() {
	verbose, logDir, rest := extractGlobalFlags(os.Args[1:])
	if len(rest) == 0 {
		printUsage()
		os.Exit(1)
	}

	runID := newRunID(logDir, auditDir, time.Now())
	ctx := telemetry.WithRunID(context.Background(), runID)

	cleanup, err := telemetry.Init(telemetry.Config{
		Verbose: verbose,
		LogDir:  logDir,
		RunID:   runID,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed initializing logger in %s: %v\n", logDir, err)
		os.Exit(1)
	}
	defer cleanup()

	log := telemetry.Log(ctx)
	started := time.Now()
	log.Info("run started", "version", version, "command", rest[0], "args", rest[1:], "log_dir", logDir, "verbose", verbose)

	// Artifact rotation (best-effort, never fatal): per-run logs/audit dirs
	// accumulate on every invocation, so prune past the newest
	// maxRetainedRuns before dispatch. Ledger/state/staged are resume
	// state, never prune targets.
	if n := pruneOldRuns(logDir, auditDir, maxRetainedRuns); n > 0 {
		log.Info("artifact rotation pruned old runs", "removed", n, "keep", maxRetainedRuns)
	}

	commands := map[string]func(context.Context, []string) error{
		"extract":        runExtract,
		"plan":           runPlan,
		"convertgo":      runConvert,
		"convertcs":      runConvertcs,
		"discover":       runDiscover,
		"ainames":        runAINames,
		"convertbatchpy": runBatchpy,
		"pygentest":      runPygentest,
		"analyze":        runAnalyze,
		"gentest":        runGentest,
		"flow":           runFlow,
		"retrystats":     runRetryStats,
		"templates":      runTemplates,
		"dbcheck":        runDBCheck,
		"walkreport":     runWalkreport,
	}
	run, ok := commands[rest[0]]
	switch {
	case ok:
		if err := run(ctx, rest[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				// -h/--help is a usage request, not a failure —
				// every subcommand uses ContinueOnError FlagSets.
				os.Exit(0)
			}
			log.Error(rest[0]+" failed", "error", err)
			os.Exit(1)
		}
	case rest[0] == "version":
		fmt.Printf("tuxconv version %s\n", version)
	case rest[0] == "help" || rest[0] == "-h" || rest[0] == "--help":
		printUsage()
	default:
		log.Error("unknown command", "command", rest[0])
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", rest[0])
		printUsage()
		os.Exit(1)
	}

	log.Info("run completed", "command", rest[0], "duration_s", elapsedSeconds(started))
}

// elapsedSeconds is the end-of-run duration in seconds, rounded to two
// decimals (millisecond precision logged as 5-digit raw numbers was
// unreadable in the run summaries).
func elapsedSeconds(t time.Time) float64 {
	return math.Round(time.Since(t).Seconds()*100) / 100
}

// newRunID returns the run identifier: local time as DDMMYYYY_HHMMSS (easy
// to eyeball and sort), with a -N suffix when a same-second run already left
// artifacts (log file or audit folder), so repeat runs never collide.
func newRunID(logDir, auditRoot string, now time.Time) string {
	base := now.Format("02012006_150405")
	runID := base
	for i := 2; ; i++ {
		taken := false
		if logDir != "" {
			if _, err := os.Stat(filepath.Join(logDir, "run-"+runID+".jsonl")); err == nil {
				taken = true
			}
		}
		if !taken && auditRoot != "" {
			if _, err := os.Stat(filepath.Join(auditRoot, runID)); err == nil {
				taken = true
			}
		}
		if !taken {
			return runID
		}
		runID = fmt.Sprintf("%s-%d", base, i)
	}
}

// extractGlobalFlags pulls the run-wide flags (-verbose, -log-dir) out of the
// argument list wherever they appear, leaving the subcommand and its flags in
// rest. `--` passes everything after it through untouched.
func extractGlobalFlags(args []string) (verbose bool, logDir string, rest []string) {
	logDir = defaultLogDir
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		name := strings.TrimLeft(a, "-")
		value := ""
		hasValue := false
		if eq := strings.Index(name, "="); eq >= 0 {
			value, name, hasValue = name[eq+1:], name[:eq], true
		}
		switch name {
		case "verbose":
			if hasValue {
				b, err := strconv.ParseBool(value)
				if err != nil {
					rest = append(rest, a)
					continue
				}
				verbose = b
				continue
			}
			verbose = true
			continue
		case "log-dir":
			if hasValue {
				logDir = value
				continue
			}
			if i+1 < len(args) {
				i++
				logDir = args[i]
				continue
			}
		}
		rest = append(rest, a)
	}
	return verbose, logDir, rest
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `tuxconv — tux→Go conversion on the tree-sitter parse stack

Usage:
  tuxconv [global flags] <command> [arguments]

Global Flags:
  -verbose             enable debug-level console logging (file log always captures debug)
  -log-dir <dir>       structured JSONL log directory (default conversion_logs/logs)

Available Commands:
  extract      Extract the deterministic IR (query units + QueryType marking, condition
               inventory, FML ops, external fns) as JSON; every file's IR is archived
               under conversion_logs/audit/<run-id>/
  plan         Generate the deterministic decomposition plan from the IR + the
               user's endpoint mapping (plan.json/plan.md in the ledger dir)
  convertgo    Convert a .pc/.pcf file or directory into the target Go service.
               Without a mapping it first scans the target and writes editable
               mapping drafts to mappings/ and stops — review, then re-run
  convertbatchpy
               Convert Pro*C batch programs into Python service modules
               (SQL constants + repository/DAL + service with process_daily_batch;
               pychk syntax gate + SQL fidelity + retention report)
  pygentest    Generate AND RUN the assertion suite for batch Python modules
               already written by convertbatchpy (Tier 1 SQL well-formedness,
               Tier 2 repository contract, Tier 3 service lifecycle, Tier 4
               negatives; stdlib-only, no database). The exit code is the gate:
               a suite with findings fails. -no-run writes without executing;
               -strict also fails on not-asserted claims.
  convertcs    Convert a .pc/.pcf file into the .NET Core (C#) component tree
               (Controller / DTO / NamedQueries / Repository / Service;
                without a mapping it first scans the target and writes
                editable <stem>.cs.mapping.yaml drafts to mappings/ and
                stops — review, then re-run; -no-llm keeps tuxconv:TODO seams)
  discover     Endpoint scan-then-tag: write a mapping draft per entry to
               mappings/ (default; -out overrides, -stdout prints).
               -target cs drafts the .NET Core mapping schema
               (<name>.cs.mapping.yaml: namespace/area/component
               placeholders, scenario endpoints, requestFields/paramNames,
               dbMethods pins).
               -list-axes prints the entry's dispatch-axis registry;
               -filter "<expr>" previews a scenarioFilter fold (read-only,
               no drafts/artifacts)
               -filter-json <path> writes the same preview as JSON with the
               flattened source (web playground; needs -filter)
  ainames      AI naming pass over an edited mapping yaml: names ONLY the surviving
               endpoints (name/route + dbMethods pins) and patches the yaml in
               place, preserving all comments and user edits. Skips entries
               already named ai-suggested (-all re-names); requires run.llm.
               Pairs with "discover -no-llm": deterministic draft, prune,
               ainames, convertgo
  analyze      Analyze Pro*C/Tuxedo complexity (+1/+5/+10/+20 rubric) and export CSV
               (selectors: analyze folder/file.pc, analyze folder file.pc,
               analyze folder "a.pc, b.pc" / a b c, or a .txt file list)
  gentest      Generate db/controller/handler Go tests for a converted service
               tree (-check-only reports the gap; -no-llm template-deterministic)
  flow         Inspect per-function flow trees (coverage, idiom hints, -go
               draft) and optionally emit scenario artifacts (-scenarios);
               read-only, no staging, no LLM
  retrystats   Compare the retry behaviour of one or two audit runs (attempts,
               outcomes, tokens per unit) — the retry methodology A/B read-out:
               retrystats conversion_logs/audit/<runA> [<runB>]
  templates    User template overlay: list (effective set + origins), dump
               (export the embedded set to a directory), verify (check an
               override dir for unknown ids / parse errors before a run)
  dbcheck      Optional Oracle probe (internal/db, database/sql): reports
               enabled/driver/source without connecting; -ping verifies.
               No DSN = offline (exit 0) — conversion never needs the DB
  walkreport   Walk-faithful progress read-out: per-function flow coverage
               plus a census of the emitted tree's tuxconv:TODO gaps by reason
               code (-controller DIR, default the staged tree). Read-only;
               never calls an LLM. -json / -out for the machine form, -strict
               to fail on a finding
  version      Print version information

`)
}

// reorderArgs separates flag tokens (with their values) from positional
// arguments so flags may appear before or after the target path. Value flags
// come from the declarative table in flags.go.
func reorderArgs(args []string) (flagArgs, positional []string) {
	valueFlags := deriveValueFlags()
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			name := strings.TrimLeft(a, "-")
			if !strings.Contains(name, "=") && valueFlags[name] && i+1 < len(args) {
				i++
				flagArgs = append(flagArgs, args[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	return flagArgs, positional
}

// deriveValueFlags reports the flag names that consume a following value,
// from the declarative table in flags.go (the one home of that knowledge —
// keep it in sync when adding a flag).
func deriveValueFlags() map[string]bool {
	valueFlags := map[string]bool{}
	for _, names := range commandFlags {
		for _, name := range names {
			valueFlags[name] = true
		}
	}
	return valueFlags
}
