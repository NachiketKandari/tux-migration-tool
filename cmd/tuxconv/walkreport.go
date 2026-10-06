package main

import (
	"context"
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
	"tux-migration-tool/internal/walkreport"
)

// runWalkreport implements `tuxconv walkreport <corpus> [-controller DIR]`.
// It is the walk plan's P0 instrument (docs/deterministic-walk-plan.md), and
// it is the subcommand the plan left as an open shape question: a new
// subcommand rather than a `flow -report` flag, because the census reads
// EMITTED OUTPUT rather than the source, and folding that into `flow` would
// have made a source-analysis command quietly depend on a conversion having
// already been run.
//
// Two independent measurements, either of which may be requested alone:
//
//	<corpus>            per-function flow coverage — how much of the C walk
//	                    we UNDERSTAND (the parser-accuracy guard)
//	-controller DIR     a census of every tuxconv:TODO in an emitted
//	                    controller tree, bucketed by reason code — how much
//	                    of what we understood we could RENDER
//
// Inspection only: nothing is staged, no LLM is called (the census reads
// text and the coverage comes from the deterministic parser), and no pipeline
// state changes. -no-llm is therefore implied rather than offered — there is
// no seam here for it to govern.
func runWalkreport(ctx context.Context, args []string) error {
	log := telemetry.Log(ctx)
	fs := flag.NewFlagSet("walkreport", flag.ContinueOnError)
	controller := fs.String("controller", "", "Emitted controller tree to census (default: the conventional staged path, skipped if absent)")
	outPath := fs.String("out", "", "Write the report JSON to this path (default: print the text report)")
	jsonOut := fs.Bool("json", false, "Print the report as JSON instead of the text summary")
	configPath := fs.String("config", "", "Path to .tuxconv.yaml (default: ./.tuxconv.yaml when present, else defaults)")
	failOnFindings := fs.Bool("strict", false, "Exit non-zero when the census has findings (e.g. an unclassified TODO shape)")

	flagArgs, positional := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(positional) == 0 {
		return fmt.Errorf("must provide a .pc/.pcf file or a directory to walk-report")
	}
	target := positional[0]

	cfg, _, err := loadRunConfig(*configPath)
	if err != nil {
		return err
	}

	report, err := walkreportCorpus(ctx, target, cfg, *controller)
	if err != nil {
		return err
	}

	if *jsonOut || *outPath != "" {
		data, err := walkreport.MarshalJSON(report)
		if err != nil {
			return err
		}
		if *outPath == "" {
			fmt.Println(string(data))
		} else if err := writeWalkreportJSON(*outPath, data); err != nil {
			return err
		}
		if *outPath != "" {
			log.Info("walkreport json written", "path", *outPath)
		}
	} else {
		var b strings.Builder
		report.Render(&b)
		fmt.Print(b.String())
	}

	findings := report.Tidy()
	for _, f := range findings {
		// Findings are about the INSTRUMENT or about an unregistered gap
		// shape, not about the conversion failing. That distinction decides
		// the exit code: -strict is for CI, where an unclassified shape
		// must not pass unnoticed.
		log.Warn("walkreport finding", "finding", f)
		if *failOnFindings {
			return fmt.Errorf("walkreport: %s", findings[0])
		}
	}
	log.Info("walkreport complete", "target", target,
		"files", len(report.Coverage), "gaps", censusTotal(report))
	return nil
}

// walkreportCorpus measures one corpus: flow coverage from the source, and a
// census from the emitted tree when one is found.
//
// The census path defaults to the conventional staged location rather than
// being required as a flag, because the common question is "how did the last
// run go" and making the user locate the output would answer a different
// question. A missing tree is not an error — coverage alone is a complete
// request, and Tidy says so rather than the command pretending it measured
// gaps.
func walkreportCorpus(ctx context.Context, target string, cfg *config.Config, controller string) (*walkreport.WalkReport, error) {
	log := telemetry.Log(ctx)
	report := &walkreport.WalkReport{Target: target}

	irFiles, err := extractFlowIR(target, cfg)
	if err != nil {
		return nil, err
	}
	if len(irFiles) == 0 {
		return nil, fmt.Errorf("no .pc or .pcf files found in %s", target)
	}

	for _, f := range irFiles {
		src, err := os.ReadFile(f.Path)
		if err != nil {
			return nil, fmt.Errorf("walkreport: read %s: %w", f.Path, err)
		}
		facts, err := flow.ScanForIR(string(src), f)
		if err != nil {
			return nil, fmt.Errorf("walkreport: scan %s: %w", f.Path, err)
		}
		cf := walkreport.CoverageReport{Path: f.Path}
		// Every function, not just the entry: the helper bodies are where
		// P2 works, and `flow`'s own CLI narrows to the entry.
		for _, fn := range walkreportTargets(facts) {
			tree := flow.Build(src, facts, fn, f)
			cov := tree.Coverage
			cf.Functions = append(cf.Functions, walkreport.NewCoverage(
				fn, cov.Classified, cov.CodeLines, cov.Unknown, cov.Residue))
		}
		report.Coverage = append(report.Coverage, cf)
	}

	tree := controller
	if tree == "" {
		tree = defaultStagedController(cfg)
	}
	if tree == "" {
		return report, nil
	}
	if _, err := os.Stat(tree); err != nil {
		log.Info("no emitted controller tree to census", "path", tree,
			"hint", "run `tuxconv convertgo <target> -no-llm` first")
		return report, nil
	}
	c, err := walkreport.CensusDir(tree)
	if err != nil {
		return nil, err
	}
	report.Controller = tree
	report.Census = &c
	return report, nil
}

// walkreportTargets picks the functions to measure: every function in the
// file, sorted. Unlike flow's flowTargets, an entry never narrows the set —
// coverage of the helper bodies is the number P2 works against, and the entry
// is only one of the functions in the walk.
func walkreportTargets(facts *tsscan.SourceFacts) []string {
	out := make([]string, 0, len(facts.Functions))
	for _, fn := range facts.Functions {
		out = append(out, fn.Name)
	}
	sort.Strings(out)
	return out
}

// defaultStagedController returns the conventional emitted-tree path, or ""
// when the config does not name one. It is a convention, not a guarantee —
// a run with a custom output dir simply reports coverage only, which Tidy
// states plainly.
func defaultStagedController(cfg *config.Config) string {
	if cfg.Paths.Staged == "" {
		return ""
	}
	return cfg.Paths.Staged
}

func censusTotal(r *walkreport.WalkReport) int {
	if r.Census == nil {
		return 0
	}
	return r.Census.Total
}

func writeWalkreportJSON(path string, data []byte) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("walkreport: create %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("walkreport: write %s: %w", path, err)
	}
	return nil
}
