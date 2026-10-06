package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/flow"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/llm"
	"tux-migration-tool/internal/plan"
	"tux-migration-tool/internal/telemetry"
)

// ainames implements `tuxconv ainames <entry-file> -mapping <yaml> [-all]`
// — the naming half of the deterministic-draft workflow. Phase 1
// (`discover -no-llm`) emits a zero-AI draft; phase 2 is the user's manual
// prune; phase 3 is this command: it names ONLY the surviving entries
// (endpoint name/route plus their dbMethods pins), patching the yaml
// in place line-by-line so every comment, census note, and user edit
// outside the touched values survives byte-for-byte. Phase 4 (`convertgo`)
// is unchanged.
//
// An entry already carrying an `# ai-suggested` name is skipped (its name
// is the user's or a prior pass's decision) unless -all re-names it. A
// naming failure degrades to the deterministic picker for that entry —
// the pass always produces a loader-legal yaml.
func runAINames(ctx context.Context, args []string) error {
	log := telemetry.Log(ctx)
	fs := flag.NewFlagSet("ainames", flag.ContinueOnError)
	mappingPath := fs.String("mapping", "", "Mapping YAML to name in place (required)")
	all := fs.Bool("all", false, "Re-name entries already marked ai-suggested")
	configPath := fs.String("config", "", "Path to .tuxconv.yaml (default: ./.tuxconv.yaml when present, else defaults)")
	flagArgs, positional := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if *mappingPath == "" {
		return fmt.Errorf("ainames: -mapping <yaml> is required")
	}
	if len(positional) != 1 {
		return fmt.Errorf("ainames: pass exactly one entry file (the .pc the mapping converts)")
	}
	cfg, cfgSource, err := loadRunConfig(*configPath)
	if err != nil {
		return err
	}
	logConfigRouting(ctx, cfg, cfgSource)
	client := resolveLLMClient(ctx, cfg, false, "ai naming")
	if client == nil {
		return fmt.Errorf("ainames: no LLM client (set run.llm: true in .tuxconv.yaml) — `discover -no-llm` drafts already carry deterministic names")
	}
	w := newWiring(ctx, cfg)

	mapping, err := plan.LoadMapping(*mappingPath)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(*mappingPath)
	if err != nil {
		return err
	}

	entry := positional[0]
	files, err := extractFlowIR(entry, cfg)
	if err != nil {
		return err
	}
	var f *ir.File
	for _, cand := range files {
		if filepath.Base(cand.Path) == filepath.Base(entry) {
			f = cand
			break
		}
	}
	if f == nil {
		return fmt.Errorf("ainames: no IR extracted for %s", entry)
	}
	if f.Entry == "" {
		return fmt.Errorf("ainames: %s has no entry function — nothing to name", entry)
	}
	src, err := os.ReadFile(f.Path)
	if err != nil {
		return err
	}
	facts, err := flow.ScanForIR(string(src), f)
	if err != nil {
		return err
	}
	tree := flow.Build(src, facts, f.Entry, f)

	// The naming context mirrors discoverCore exactly: the axis fold for
	// scenarioRef endpoints, the candidate census for condition ones.
	var scens []*flow.Scenario
	var diff *flow.ScenarioDiff
	axis := tree.DispatchAxisFor(src)
	if axis != nil {
		scens = flow.Scenarios(tree, axis)
		diff = flow.DiffScenarios(f.Entry, scens)
	}
	scenByKey := map[string]*flow.Scenario{}
	for _, sc := range scens {
		scenByKey[sc.Key] = sc
	}
	candByKey := map[string]flow.Candidate{}
	for _, c := range flow.Discover(tree, f.Conditions) {
		candByKey[c.Key] = c
	}

	// Which blocks still need naming: every mapped endpoint whose name
	// line is not yet ai-suggested (or all of them under -all).
	lines := strings.Split(string(raw), "\n")
	blocks := endpointBlocks(lines)
	need := map[string]bool{} // refKey → naming required
	for _, b := range blocks {
		if _, ok := endpointForRef(mapping, b); ok && (*all || !b.aiNamed) {
			need[b.refKey()] = true
		}
	}

	queriesByID := make(map[string]*ir.Query, len(f.Queries))
	for _, q := range f.Queries {
		queriesByID[q.ID] = q
	}

	// Existing names occupy the uniqueness space the loader enforces.
	suggestions := map[string]aiSuggestion{}
	usedNames := map[string]bool{}
	for _, e := range mapping.Endpoints {
		if e.Name != "" {
			usedNames[e.Name] = true
		}
	}

	log.Info("ai naming pass started", "mapping", *mappingPath, "endpoints", len(mapping.Endpoints), "to_name", len(need))
	for _, e := range mapping.Endpoints {
		key := endpointRefKey(e)
		if !need[key] {
			continue
		}
		sug, err := nameEndpoint(ctx, log, client, w, f, tree, e, scenByKey, diff, candByKey, queriesByID, src)
		if err != nil {
			return err
		}
		// Loader-legal uniqueness: a colliding proposal gains a numeric
		// suffix — advisory still, always editable.
		sug.Name = uniqueEndpointName(sug.Name, "", usedNames)
		suggestions[key] = sug
		log.Info("ai naming proposal", "ref", key, "name", sug.Name, "route", sug.Route, "db_pins", len(sug.Methods))
	}
	dedupeRowNames(suggestions, queriesByID, log)

	if len(suggestions) == 0 {
		fmt.Println("ainames: nothing to name — every mapped entry already carries an ai-suggested name (-all to re-name)")
		return nil
	}

	patched := patchMappingYAML(string(raw), blocks, suggestions)
	if err := os.WriteFile(*mappingPath, []byte(patched), 0o644); err != nil {
		return fmt.Errorf("ainames: write %s: %w", *mappingPath, err)
	}
	log.Info("ai naming pass completed", "mapping", *mappingPath, "named", len(suggestions), "yaml_patched_in_place", true)
	fmt.Printf("ainames: named %d endpoint(s) in %s — review the diff, then convertgo\n", len(suggestions), *mappingPath)
	return nil
}

// nameEndpoint resolves one mapped endpoint's suggestion: the scenario
// naming call (census + unique blocks) for scenarioRef entries, the
// branch naming call for condition/conditionRef ones — the exact seams
// discover uses, so the audit trail and prompts never fork. A failure
// degrades to the deterministic picker.
func nameEndpoint(
	ctx context.Context, log *slog.Logger, client llm.Client, w *runWiring,
	f *ir.File, tree *flow.Tree, e plan.Endpoint,
	scenByKey map[string]*flow.Scenario, diff *flow.ScenarioDiff,
	candByKey map[string]flow.Candidate, queriesByID map[string]*ir.Query, src []byte,
) (aiSuggestion, error) {
	switch {
	case e.ScenarioRef != "":
		sc := scenByKey[e.ScenarioRef]
		if sc == nil {
			return aiSuggestion{}, fmt.Errorf("ainames: endpoint %s references scenario %q — not in the re-derived axis (stale mapping?)", e.Name, e.ScenarioRef)
		}
		sug, err := aiNameScenarioOne(ctx, log, client, w.budget, f, sc, queriesByID, strings.Split(string(src), "\n"), diff, w.audit)
		if err != nil {
			log.Warn("ai scenario naming failed — deterministic names used", "scenario", sc.Key, "error", err)
			return deterministicSuggestion(scenarioQueryIDs(sc), sc.Adds, sc.Gets, "Endpoint"+common.CamelGo(sc.Value)), nil
		}
		return sug, nil
	case e.ScenarioFilter != "":
		// Scenario filter (scenario-filter plan §5): the filter's re-fold
		// is the census the naming seam consumes, exactly like a
		// scenarioRef slice.
		filter, err := flow.ParseScenarioFilter(e.ScenarioFilter)
		if err != nil {
			return aiSuggestion{}, fmt.Errorf("ainames: endpoint %s: %w", e.Name, err)
		}
		sc, err := flow.ScenarioForFilter(tree, tree.AxesFor(src), filter)
		if err != nil {
			return aiSuggestion{}, fmt.Errorf("ainames: endpoint %s: %w", e.Name, err)
		}
		sug, err := aiNameScenarioOne(ctx, log, client, w.budget, f, sc, queriesByID, strings.Split(string(src), "\n"), diff, w.audit)
		if err != nil {
			log.Warn("ai scenario naming failed — deterministic names used", "scenario", sc.Key, "error", err)
			return deterministicSuggestion(scenarioQueryIDs(sc), sc.Adds, sc.Gets, "Endpoint"+common.CamelGo(sc.Value)), nil
		}
		return sug, nil
	default:
		cond, err := endpointCondition(f, tree, e)
		if err != nil {
			return aiSuggestion{}, err
		}
		sug, err := aiNameOne(ctx, log, client, w.budget, f, cond, queriesByID, strings.Split(string(src), "\n"), w.audit)
		if err != nil {
			log.Warn("ai naming failed — deterministic names used", "endpoint", e.Name, "error", err)
			fallback := "Endpoint" + strconv.Itoa(cond.Index)
			if c, ok := candByKey[e.ConditionRef]; ok {
				return deterministicSuggestion(c.QueryIDs, c.Adds, c.Gets, fallback), nil
			}
			gets, adds, _ := flow.Census(cond)
			return deterministicSuggestion(cond.QueryIDs, adds, gets, fallback), nil
		}
		return sug, nil
	}
}

func endpointCondition(f *ir.File, tree *flow.Tree, e plan.Endpoint) (*ir.Condition, error) {
	if e.ConditionRef != "" {
		return flow.ConditionFor(tree, f.Conditions, e.ConditionRef)
	}
	c := f.Condition(e.Condition)
	if c == nil {
		return nil, fmt.Errorf("ainames: endpoint %s maps condition %d — inventory has %d", e.Name, e.Condition, len(f.Conditions))
	}
	return c, nil
}
