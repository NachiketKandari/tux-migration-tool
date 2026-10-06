package main

// convertcs — the Pro*C → .NET Core (C#) target. scan → IR → flow →
// csplan → csgen → cschk gates → the seven-file component tree
// (Controller / DTO / NamedQueries / Repository / Service). The service
// body renders deterministically (repo calls, row mapping, logging);
// the arm's residual logic fills through the LLM seam on an LLM-enabled
// run (-no-llm keeps the tuxconv:TODO placeholder for a resume). Filled
// bodies persist in the run's ledger and are never re-generated.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"tux-migration-tool/internal/config"
	"tux-migration-tool/internal/cschk"
	"tux-migration-tool/internal/csdraft"
	"tux-migration-tool/internal/csgen"
	"tux-migration-tool/internal/csplan"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/telemetry"
)

func runConvertcs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("convertcs", flag.ContinueOnError)
	outDir := fs.String("out", "", "Output root for the generated component tree (default: convertcs.out from config, else conversion_logs/_staged)")
	mappingFlag := fs.String("mapping", "", "convertcs mapping YAML, or a directory of per-service yamls (each with source: <entry file>); empty drafts-and-stops like convertgo (default: convertcs.mapping from config, else the mappings/ convention)")
	noLLM := fs.Bool("no-llm", false, "deterministic-only run (service bodies keep tuxconv:TODO seams; overrides run.llm)")
	configPath := fs.String("config", "", "Path to .tuxconv.yaml (default: ./.tuxconv.yaml when present, else defaults)")
	templatesDir := fs.String("templates", "", "Directory of <template_id>.tmpl overrides (flag > templates.dir config; missing ids keep the embedded set)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: tuxconv convertcs <file|dir> [-mapping <yaml|dir>] [-no-llm] [-out dir] [-config path] [-templates dir]")
		fs.PrintDefaults()
	}
	flagArgs, positional := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	log := telemetry.Log(ctx)
	cfg, cfgSource, err := loadRunConfig(*configPath)
	if err != nil {
		return err
	}
	logConfigRouting(ctx, cfg, cfgSource)

	tpl, err := templateProvider(ctx, cfg, *templatesDir)
	if err != nil {
		return err
	}

	// Flag > config > default precedence (C1), mirroring convertgo:
	// -mapping wins, else convertcs.mapping, else the mappings/ convention
	// when it holds a cs draft. Nothing found → draft-and-stop: scan the
	// target, write <stem>.cs.mapping.yaml drafts, and stop so the user's
	// review is a free re-run (the tool never invents endpoints).
	target, err := resolveConvertcsInput(positional, cfg)
	if err != nil {
		return err
	}
	mappingPath := mappingForCs(*mappingFlag, cfg)
	if mappingPath == "" {
		return draftAndStopCs(ctx, target, cfg)
	}
	noLLMEnabled := *noLLM || cfg.Convertcs.NoLLM

	fi, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("convertcs: cannot access %s: %w", target, err)
	}
	path := target
	if fi.IsDir() {
		// A directory mapping for a single-entry target: pick the yaml
		// whose source:/stem matches the entry (the mappings/ convention).
		// Nothing matches — including a convention dir holding only foreign
		// (Go) drafts — and the run drafts-and-stops instead.
		if st, serr := os.Stat(mappingPath); serr == nil && st.IsDir() {
			entryBase, derr := convertcsDirEntry(target, mappingPath)
			if derr != nil {
				return derr
			}
			mappingPath, err = mappingForCsEntry(mappingPath, entryBase)
			if err != nil {
				return err
			}
			if mappingPath == "" {
				return draftAndStopCs(ctx, target, cfg)
			}
		}
		path, err = resolveDirSource(target, mappingPath)
		if err != nil {
			return err
		}
	}

	log.Info("convertcs started", "source", path, "mapping", mappingPath, "no_llm", noLLMEnabled)

	mapping, err := csplan.LoadMapping(mappingPath)
	if err != nil {
		return err
	}
	irf, err := ir.ExtractFileOpts(path, irOptions(cfg, false))
	if err != nil {
		return fmt.Errorf("convertcs: extract %s: %w", path, err)
	}
	if irf.Entry == "" {
		return fmt.Errorf("convertcs: %s has no Tuxedo entry — fn libraries convert via convertgo (fn-lib mode)", filepath.Base(path))
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("convertcs: read %s: %w", path, err)
	}

	plan, err := csplan.Build(csplan.Options{Main: irf, Source: string(src), Mapping: mapping})
	if err != nil {
		return err
	}

	client := resolveLLMClient(ctx, cfg, noLLMEnabled, "convertcs service bodies")
	wiring := newWiring(ctx, cfg)

	// Archive the plan (engine-wiring audit Tier-1 #7): the decomposition —
	// scenario refs, the loud SCEN-D4 slice residue, line spans, requests —
	// is computed here and never landed in the audit trail. Best-effort,
	// like every archive.
	if _, err := wiring.audit.WriteJSON(plan.Component+"_csplan.json", plan); err != nil {
		log.Warn("convertcs plan archive write failed", "error", err)
	} else {
		log.Info("convertcs plan archived", "component", plan.Component)
	}

	// Ledger resume (A4): filled bodies are never re-generated — a re-run
	// re-gates and reuses them; TODO seams retry the seam.
	ledgerPath := convertcsLedgerPath(cfg, plan.Component)
	resumed := loadConvertcsLedger(ledgerPath, plan.Source)

	res, err := csgen.Generate(ctx, csgen.Options{
		Plan: plan, Source: string(src), NoLLM: noLLMEnabled || client == nil,
		Client: client, Budget: wiring.budget, MaxRetries: cfg.ValidateCfg.MaxRetries,
		Audit: wiring.audit, Resumed: resumed, Templates: tpl,
	})
	if err != nil {
		return err
	}
	saveConvertcsLedger(ctx, ledgerPath, plan, res)

	// Gates: structural per file + SQL fidelity + Oracle param counts.
	var issues []cschk.Issue
	typeNames := map[string]string{
		"Controller/" + plan.Controller + ".cs":   plan.Controller,
		"DTO/" + plan.DTOCls + ".cs":              plan.DTOCls,
		"NamedQueries/" + plan.QueriesCls + ".cs": plan.QueriesCls,
		"Repository/I" + plan.Repo + ".cs":        "I" + plan.Repo,
		"Repository/" + plan.Repo + ".cs":         plan.Repo,
		"Service/I" + plan.Service + ".cs":        "I" + plan.Service,
		"Service/" + plan.Service + ".cs":         plan.Service,
	}
	for _, rel := range res.Order {
		issues = append(issues, cschk.Check(rel, res.Files[rel], typeNames[rel])...)
	}
	issues = append(issues, cschk.SQLFidelity(plan, res.Files)...)
	issues = append(issues, cschk.OracleParams(plan, res.Files)...)

	// Write the tree under the output root, rooted at the component.
	base := *outDir
	if base == "" {
		base = cfg.Convertcs.Out
	}
	if base == "" {
		base = config.DefaultStagedDir
	}
	root := filepath.Join(base, plan.Component)
	for _, rel := range res.Order {
		dst := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("convertcs: mkdir %s: %w", filepath.Dir(dst), err)
		}
		if err := os.WriteFile(dst, []byte(res.Files[rel]), 0o644); err != nil {
			return fmt.Errorf("convertcs: write %s: %w", dst, err)
		}
		log.Info("convertcs file written", "file", dst)
	}

	deviations := 0
	structural := 0
	for _, is := range issues {
		if is.Kind == "sql-deviation" {
			deviations++
		} else {
			structural++
		}
		log.Error("convertcs gate issue", "kind", is.Kind, "file", is.File, "detail", is.Detail)
	}

	for _, w := range plan.Warnings {
		log.Warn("convertcs arm coverage", "detail", w)
		fmt.Println("  coverage:", w)
	}
	for _, n := range res.Notes {
		fmt.Println("  note:", n)
	}

	log.Info("convertcs completed", "component", plan.Component, "files", len(res.Order),
		"endpoints", len(plan.Endpoints), "queries", len(plan.Queries),
		"sql_deviations", deviations, "structural_issues", structural,
		"llm_calls", res.LLMCalls, "filled_bodies", len(res.Filled))
	fmt.Printf("convertcs: %d file(s) written under %s — %d endpoint(s), %d query unit(s), %d sql deviations, %d structural issues, %d llm calls\n",
		len(res.Order), root, len(plan.Endpoints), len(plan.Queries), deviations, structural, res.LLMCalls)
	if deviations+structural > 0 {
		return fmt.Errorf("convertcs: %d gate issue(s) — review the log above", deviations+structural)
	}
	return nil
}

// mappingForCs resolves the convertcs endpoint mapping: the -mapping flag
// wins, else convertcs.mapping from the yaml, else the mappings/ convention
// when that directory holds at least one cs draft
// (<stem>.cs.mapping.yaml). "" = nothing found (the caller then
// drafts-and-stops). Go drafts never satisfy it — a convention dir holding
// only Go mappings drafts-and-stops too.
func mappingForCs(flagValue string, cfg *config.Config) string {
	if strings.TrimSpace(flagValue) != "" {
		return flagValue
	}
	if strings.TrimSpace(cfg.Convertcs.Mapping) != "" {
		return cfg.Convertcs.Mapping
	}
	if fi, err := os.Stat(defaultMappingsDir); err == nil && fi.IsDir() && hasCsMappingYamls(defaultMappingsDir) {
		return defaultMappingsDir
	}
	return ""
}

// hasCsMappingYamls reports whether dir holds at least one cs mapping draft:
// <stem>.cs.mapping.yaml (or .yml), or any yaml whose stem ends in .cs.
func hasCsMappingYamls(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lower := strings.ToLower(name)
		switch filepath.Ext(lower) {
		case ".yaml", ".yml":
		default:
			continue
		}
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		if strings.HasSuffix(strings.ToLower(stem), ".cs.mapping") || strings.HasSuffix(strings.ToLower(stem), ".cs") {
			return true
		}
	}
	return false
}

// mappingForCsEntry picks one cs mapping yaml out of a directory for a
// single-entry convert: a mapping whose source: names the entry file wins,
// else the one whose stem matches the entry stem (tolerating the
// .cs.mapping.yaml double extension the draft convention uses). Matching is
// lenient — only the source field is read, so Go drafts in the shared
// convention dir never poison the run — and only the winner is validated
// (by the caller's LoadMapping). Zero matches returns "" so the caller
// drafts-and-stops; two matches refuse to guess.
func mappingForCsEntry(dir, entryBase string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("convertcs: read mapping dir %s: %w", dir, err)
	}
	var bySource, byStem []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".yaml", ".yml":
		default:
			continue
		}
		path := filepath.Join(dir, e.Name())
		src, serr := csplan.MappingSourceOf(path)
		if serr != nil {
			return "", serr
		}
		if src != "" && strings.EqualFold(filepath.Base(src), entryBase) {
			bySource = append(bySource, path)
		}
		// Stem match tolerates both conventions:
		// SVC_X.cs.mapping.yaml ↔ SVC_X.pc and SVC_X.mapping.yaml ↔ SVC_X.pc.
		nameStem := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		nameStem = strings.TrimSuffix(nameStem, ".mapping")
		nameStem = strings.TrimSuffix(nameStem, ".cs")
		if nameStem == stemOf(entryBase) {
			// Only cs-suffixed drafts count for stem matches, so a Go
			// draft for the same service never wins a convertcs run.
			rawStem := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
			if strings.HasSuffix(strings.ToLower(rawStem), ".cs.mapping") || strings.HasSuffix(strings.ToLower(rawStem), ".cs") {
				byStem = append(byStem, path)
			}
		}
	}
	switch {
	case len(bySource) == 1:
		return bySource[0], nil
	case len(bySource) > 1:
		return "", fmt.Errorf("convertcs: mappings %s and %s both declare source %s — pass -mapping <file> explicitly", bySource[0], bySource[1], entryBase)
	case len(byStem) == 1:
		return byStem[0], nil
	case len(byStem) > 1:
		return "", fmt.Errorf("convertcs: %s holds several mappings for %s — pass -mapping <file> explicitly", dir, entryBase)
	default:
		return "", nil
	}
}

// convertcsDirEntry finds the single entry .pc/.pcf file under a directory
// target so a directory mapping can be matched to it. Multiple entries
// refuse to guess — pass -mapping <file> explicitly (one component per
// convertcs run).
func convertcsDirEntry(dir, mappingPath string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("convertcs: read dir %s: %w", dir, err)
	}
	var pcs []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".pc", ".pcf":
			pcs = append(pcs, e.Name())
		}
	}
	if len(pcs) == 1 {
		return pcs[0], nil
	}
	if len(pcs) == 0 {
		return "", fmt.Errorf("convertcs: no .pc/.pcf files found in %s", dir)
	}
	return "", fmt.Errorf("convertcs: %s holds %d entry files — pass -mapping <file> for one component (mapping dir %s cannot disambiguate)", dir, len(pcs), mappingPath)
}

// draftAndStopCs is convertcs's no-mapping fallback (draft, then stop): the
// target is scanned for dispatch-arm endpoints and editable cs mapping
// drafts land in mappings/ (deterministic — no AI pass), and the run exits
// before generating anything, so the user's review is a free re-run
// (no tree cleanup, no half-converted state).
func draftAndStopCs(ctx context.Context, target string, cfg *config.Config) error {
	n, err := discoverCsCore(ctx, target, discoverOutDir(""), false, cfg,
		csdraft.Options{Namespace: cfg.Convertcs.Namespace, Area: cfg.Convertcs.Area})
	if err != nil {
		return err
	}
	telemetry.Log(ctx).Info("convertcs: no mapping found — drafts written, conversion deferred to the re-run",
		"target", target, "drafts", n)
	return nil
}

// resolveConvertcsInput picks the conversion target: the CLI positional
// wins, else convertcs.input from the yaml, else an error naming both
// sources (C1 precedence).
func resolveConvertcsInput(positional []string, cfg *config.Config) (string, error) {
	if len(positional) > 0 {
		return positional[0], nil
	}
	if cfg.Convertcs.Input != "" {
		return cfg.Convertcs.Input, nil
	}
	return "", fmt.Errorf("no input target: pass a .pc/.pcf file or directory, or set convertcs.input in .tuxconv.yaml")
}

// resolveDirSource picks the mapping's source entry from a directory
// target (the same contract as the Go pipeline's dir-mode mapping).
func resolveDirSource(dir, mappingPath string) (string, error) {
	// read only the source: field (lenient) to find the entry file
	m, err := csplan.LoadMapping(mappingPath)
	if err != nil {
		return "", err
	}
	if m.Source == "" {
		return "", fmt.Errorf("convertcs: %s is a directory — the mapping must set source: <entry .pc file>", dir)
	}
	cand := filepath.Join(dir, m.Source)
	if _, err := os.Stat(cand); err != nil {
		return "", fmt.Errorf("convertcs: mapping source %s not found under %s", m.Source, dir)
	}
	return cand, nil
}

// csBodyLedger is the convertcs resume record (A4): per-endpoint body
// state under the run's ledger dir. Filled bodies persist so a re-run
// never re-generates them; TODO seams record filled=false so a later
// LLM-enabled run retries the seam.
type csBodyLedger struct {
	Source   string            `json:"source"`
	Endpoint map[string]csBody `json:"endpoints"`
}

type csBody struct {
	Span   string `json:"span"`
	Filled bool   `json:"filled"`
	Body   string `json:"body,omitempty"`
}

// convertcsLedgerPath is the component's resume file under paths.ledger.
func convertcsLedgerPath(cfg *config.Config, component string) string {
	return filepath.Join(cfg.Paths.Ledger, "convertcs-"+component+".json")
}

// loadConvertcsLedger returns the component's filled bodies keyed by
// endpoint name (only entries recorded against the same source file).
// Missing or corrupt files resume nothing — the run regenerates honestly.
func loadConvertcsLedger(path, source string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var led csBodyLedger
	if err := json.Unmarshal(data, &led); err != nil || led.Source != source {
		return nil
	}
	resumed := map[string]string{}
	for name, b := range led.Endpoint {
		if b.Filled && b.Body != "" {
			resumed[name] = b.Body
		}
	}
	if len(resumed) == 0 {
		return nil
	}
	return resumed
}

// saveConvertcsLedger persists the run's per-endpoint body state
// (best-effort: a ledger write failure is a WARN, never a run failure).
func saveConvertcsLedger(ctx context.Context, path string, plan *csplan.Plan, res csgen.Result) {
	led := csBodyLedger{Source: plan.Source, Endpoint: map[string]csBody{}}
	for _, ep := range plan.Endpoints {
		b := csBody{Span: ep.SourceSpan}
		if body, ok := res.Bodies[ep.Name]; ok {
			b.Filled = true
			b.Body = body
		}
		led.Endpoint[ep.Name] = b
	}
	data, err := json.MarshalIndent(led, "", "  ")
	if err != nil {
		telemetry.Log(ctx).Warn("convertcs ledger write failed", "error", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		telemetry.Log(ctx).Warn("convertcs ledger write failed", "error", err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		telemetry.Log(ctx).Warn("convertcs ledger write failed", "error", err)
	}
}
