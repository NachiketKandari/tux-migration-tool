package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"tux-migration-tool/internal/config"
	"tux-migration-tool/internal/flow"
	"tux-migration-tool/internal/telemetry"
	tsscan "tux-migration-tool/internal/tsscan"
)

// runFlow implements `tuxconv flow <file|dir>` — the accuracy instrument
// ported from convert-tux-to-go's `tuxconv flow` (the one tuxconv command the
// rename dropped): per-function flow trees (branches, loops, SQL spans,
// returns, residual statements), the coverage metric (classified vs
// residue), the idiom hints, and — with -go — the deterministic
// transpilation draft. With -scenarios it also emits the dispatch-axis
// scenario set through the same artifacts discover writes. Inspection only:
// nothing is staged, no LLM is called, no pipeline state changes.
//
// New-stack notes (vs the tuxconv original): extraction threads the run
// config's buffer registry (extractFlowIR); the draft renders via
// flow.RenderSpan over the whole function span (the RenderTree wrapper is
// gone); scenario slicing goes through DispatchAxisFor + Scenarios (the
// scenarioSliceFor fallback was deleted as never-wired).
func runFlow(ctx context.Context, args []string) error {
	log := telemetry.Log(ctx)
	fs := flag.NewFlagSet("flow", flag.ContinueOnError)
	outPath := fs.String("out", "", "Write the flow JSON to this path (defaults to stdout summary only)")
	showGo := fs.Bool("go", false, "Print the deterministic Go transpilation draft per function")
	scenarios := fs.Bool("scenarios", false, "Emit the dispatch-axis scenario artifacts (scenarios/<entry>.<var>_<val>.pc + shared report)")
	scenDir := fs.String("scenarios-dir", "", "Directory for the scenario artifacts (default: scenarios/)")
	configPath := fs.String("config", "", "Path to .tuxconv.yaml (default: ./.tuxconv.yaml when present, else defaults)")

	flagArgs, positional := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(positional) == 0 {
		return fmt.Errorf("must provide a .pc/.pcf file or a directory to flow-analyze")
	}
	target := positional[0]

	cfg, _, err := loadRunConfig(*configPath)
	if err != nil {
		return err
	}

	irFiles, err := extractFlowIR(target, cfg)
	if err != nil {
		return err
	}
	if len(irFiles) == 0 {
		return fmt.Errorf("no .pc or .pcf files found in %s", target)
	}

	scenOut := *scenDir
	if scenOut == "" {
		scenOut = config.DefaultScenDir
	}

	report := flowReport{Target: target}
	for _, f := range irFiles {
		src, err := os.ReadFile(f.Path)
		if err != nil {
			return fmt.Errorf("flow: read %s: %w", f.Path, err)
		}
		facts, err := flow.ScanForIR(string(src), f)
		if err != nil {
			return fmt.Errorf("flow: scan %s: %w", f.Path, err)
		}
		fr := flowFile{Path: f.Path}
		for _, fn := range flowTargets(facts, f.Entry) {
			tree := flow.Build(src, facts, fn, f)
			hints := flow.Match(tree)
			fr.Functions = append(fr.Functions, flowFunc{Name: fn, Tree: tree, Hints: hints})
			printFlowFn(fn, tree, hints, *showGo)
		}
		if *scenarios && f.Entry != "" {
			entryTree := flow.Build(src, facts, f.Entry, f)
			axis := entryTree.DispatchAxisFor(src)
			if axis == nil {
				fmt.Printf("\n%s scenarios: 0 (axis: none — no dispatch spine to fold; use discover for condition drafts)\n", f.Entry)
				continue
			}
			scens := flow.Scenarios(entryTree, axis)
			fmt.Printf("\n%s scenarios: %d (%s)\n", f.Entry, len(scens), axis.String())
			paths, err := scenarioArtifacts(log, scenOut, f.Entry, src, scens, f, entryTree)
			if err != nil {
				return err
			}
			for _, p := range paths {
				fmt.Printf("  %s\n", p)
			}
		} else if *scenarios {
			fmt.Printf("\n%s: fn library — scenarios are an entry-function artifact, skipped\n", filepath.Base(f.Path))
		}
		report.Files = append(report.Files, fr)
	}

	if *outPath != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(*outPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(*outPath, data, 0o644); err != nil {
			return fmt.Errorf("failed writing flow JSON to %s: %w", *outPath, err)
		}
		log.Info("flow json written", "path", *outPath)
	}
	log.Info("flow analysis complete", "files", len(report.Files))
	return nil
}

// flowTargets picks the functions to build: the IR entry when named, else
// every function (fn libraries).
func flowTargets(facts *tsscan.SourceFacts, entry string) []string {
	if entry != "" {
		return []string{entry}
	}
	var out []string
	for _, fn := range facts.Functions {
		out = append(out, fn.Name)
	}
	sort.Strings(out)
	return out
}

func printFlowFn(fn string, tree *flow.Tree, hints []flow.Hint, showGo bool) {
	cov := tree.Coverage
	pct := 100
	if cov.CodeLines > 0 {
		pct = cov.Classified * 100 / cov.CodeLines
	}
	fmt.Printf("\n%s  (lines %d-%d)\n", fn, tree.StartLine, tree.EndLine)
	fmt.Printf("  coverage: %d/%d code lines classified (%d%%), unknown %d", cov.Classified, cov.CodeLines, pct, cov.Unknown)
	if len(cov.Residue) > 0 {
		fmt.Printf(" at lines %s", joinFlowInts(cov.Residue, 8))
	}
	fmt.Println()
	for _, h := range hints {
		fmt.Printf("  hint [%s] line %d: %s\n", h.Kind, h.Line, h.Detail)
	}
	if showGo {
		out := flow.RenderSpan(tree, nil, tree.StartLine, tree.EndLine, 1)
		fmt.Println("  draft:")
		for _, line := range strings.Split(strings.TrimRight(out.Body, "\n"), "\n") {
			fmt.Printf("  %s\n", line)
		}
		for _, t := range out.TODOs {
			fmt.Printf("  gap: %s\n", t)
		}
	}
}

func joinFlowInts(xs []int, max int) string {
	if len(xs) > max {
		xs = xs[:max]
	}
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = fmt.Sprintf("%d", x)
	}
	return strings.Join(parts, ",")
}

type flowReport struct {
	Target string     `json:"target"`
	Files  []flowFile `json:"files"`
}

type flowFile struct {
	Path      string     `json:"path"`
	Functions []flowFunc `json:"functions"`
}

type flowFunc struct {
	Name  string      `json:"name"`
	Tree  *flow.Tree  `json:"tree"`
	Hints []flow.Hint `json:"hints"`
}
