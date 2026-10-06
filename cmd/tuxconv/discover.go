package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"tux-migration-tool/internal/budget"
	"tux-migration-tool/internal/common"
	"tux-migration-tool/internal/config"
	"tux-migration-tool/internal/csdraft"
	"tux-migration-tool/internal/flow"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/llm"
	"tux-migration-tool/internal/plan"
	"tux-migration-tool/internal/telemetry"
	tsscan "tux-migration-tool/internal/tsscan"
)

// runDiscover implements `tuxconv discover <file|dir>` — the PRD-2026-09-10
// endpoint-discovery scan-then-tag mode: the flow IR finds the API
// candidates (conditions/blocks enclosing Fget32 reads and non-error
// Fadd32 writes) and emits a mapping draft the user tags with names/routes.
// The tool never invents endpoints (§4.2.8). Naming: one AI call per
// candidate when the client is available (run.llm, no -no-llm), deterministic
// names otherwise — every proposal is an advisory draft default.
func runDiscover(ctx context.Context, args []string) error {
	log := telemetry.Log(ctx)
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	outDir := fs.String("out", "", "Directory for the draft yamls (default: mappings/)")
	stdout := fs.Bool("stdout", false, "Print the draft(s) instead of writing files")
	noLLM := fs.Bool("no-llm", false, "Deterministic-only run: skip AI naming (overrides run.llm)")
	configPath := fs.String("config", "", "Path to .tuxconv.yaml (default: ./.tuxconv.yaml when present, else defaults)")
	target := fs.String("target", "", "Draft dialect: go (default) or cs — the .NET Core mapping schema")
	listAxes := fs.Bool("list-axes", false, "Print each entry's ranked dispatch-axis registry (the scenarioFilter variables) — read-only, no drafts/artifacts")
	filterExpr := fs.String("filter", "", "Preview a scenarioFilter expression (merged key, matched assignments, kept blocks, queries) — read-only, no drafts/artifacts")
	filterJSON := fs.String("filter-json", "", "Write the scenarioFilter preview as JSON to this path (structured + flattened source for the web playground) — read-only, no drafts/artifacts")

	flagArgs, positional := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if *target != "" && *target != "go" && *target != "cs" {
		return fmt.Errorf("-target: got %q, want one of [go cs]", *target)
	}
	// The config always routes (defaults when the file is absent) — the
	// target falls back to convert.input so a bare `tuxconv discover` works.
	cfg, cfgSource, err := loadRunConfig(*configPath)
	if err != nil {
		return err
	}
	logConfigRouting(ctx, cfg, cfgSource)

	var path string
	if len(positional) > 0 {
		path = positional[0]
	} else {
		path = cfg.Convert.Input
	}
	if path == "" {
		return fmt.Errorf("must provide a .pc/.pcf file or directory, or set convert.input in .tuxconv.yaml")
	}

	// Read-only preview modes (scenario-filter plan §5): the axes registry
	// and the filter fast loop never need a draft, an artifact, or the LLM.
	if *listAxes || *filterExpr != "" || *filterJSON != "" {
		if *target == "cs" {
			return fmt.Errorf("-list-axes/-filter preview the Go flow tree — drop -target cs")
		}
		if *filterJSON != "" && *filterExpr == "" {
			return fmt.Errorf("-filter-json needs -filter \"<expr>\"")
		}
		return discoverPreview(path, cfg, *listAxes, *filterExpr, *filterJSON)
	}

	if *target == "cs" {
		// The cs drafts are deterministic (B1/B2): no AI naming pass —
		// every suggestion derives from the flow IR and the config
		// namespace/area defaults.
		_, err = discoverCsCore(ctx, path, discoverOutDir(*outDir), *stdout, cfg,
			csdraft.Options{Namespace: cfg.Convertcs.Namespace, Area: cfg.Convertcs.Area})
		return err
	}

	client := resolveLLMClient(ctx, cfg, *noLLM, "endpoint naming")
	if client != nil {
		if mdl, rerr := cfg.Route(""); rerr == nil {
			log.Info("AI naming enabled — one call per candidate endpoint, deterministic fallback per failure", "model", mdl.Model)
		}
	}
	bd := newWiring(ctx, cfg).budget

	_, err = discoverCore(ctx, path, discoverOutDir(*outDir), *stdout, cfg, client, bd)
	return err
}

// discoverCore is the scan-then-tag engine shared by `tuxconv discover` and
// convert's no-mapping fallback: for every entry file it finds the API
// candidates and emits an editable draft into out (or prints with stdout).
// AI naming runs per candidate when client is non-nil; the deterministic
// picker fills any gaps. Drafts never clobber — existing ones stay and the
// fresh draft lands alongside as a numbered sibling.
func discoverCore(ctx context.Context, target, out string, stdout bool, cfg *config.Config, client llm.Client, bd budget.Budget) (int, error) {
	log := telemetry.Log(ctx)

	fi, err := os.Stat(target)
	if err != nil {
		return 0, fmt.Errorf("cannot access target path %s: %w", target, err)
	}
	dirMode := fi.IsDir()

	irFiles, err := extractFlowIR(target, cfg)
	if err != nil {
		return 0, err
	}
	if len(irFiles) == 0 {
		return 0, fmt.Errorf("no .pc or .pcf files found in %s", target)
	}

	rec := auditRecorder(ctx)

	// Service names key the generated package/directory, so two drafts in
	// one tree must not share one. The uniqueness space spans this run's
	// drafts and the drafts already on disk (never-clobber means old ones
	// accumulate in out); stdout runs dedupe within the run only, keeping
	// the printed text deterministic.
	usedServices := map[string]bool{}
	if !stdout {
		usedServices = usedServiceNames(out)
	}

	written, existing := 0, 0
	for _, f := range irFiles {
		if f.Entry == "" {
			fmt.Printf("- %s: no entry function (fn library) — skipped\n", filepath.Base(f.Path))
			continue
		}
		svc := uniqueServiceName(serviceName(f), usedServices)
		usedServices[svc] = true
		src, err := os.ReadFile(f.Path)
		if err != nil {
			return written, fmt.Errorf("discover: read %s: %w", f.Path, err)
		}
		facts, err := flow.ScanForIR(string(src), f)
		if err != nil {
			return written, fmt.Errorf("discover: scan %s: %w", f.Path, err)
		}
		tree := flow.Build(src, facts, f.Entry, f)
		var draft string
		if axis := tree.DispatchAxisFor(src); axis != nil {
			// SCEN-5: the dispatch-axis entry folds into scenario slices —
			// the scenario draft + the SCEN-3/4 artifacts replace the block
			// heuristic on the endpoint path (condition/conditionRef drafts
			// stay loadable for the non-axis legacy files). The artifacts
			// are the human-validation evidence and write regardless; the
			// draft needs at least one scenario passing the census rubric
			// (FML traffic on either contract side — a reads-only slice is
			// a message-only API, the handler returns the string).
			scens := flow.Scenarios(tree, axis)
			axes := tree.AxesFor(src)
			diff := flow.DiffScenarios(f.Entry, scens)
			qualifying := 0
			for _, sc := range scens {
				if sc.CarriesContract() {
					qualifying++
				}
			}
			printScenarioSummary(f, axis, scens)
			printReturnCrossCheck(tree, facts, f)
			if qualifying == 0 {
				fmt.Printf("- %s: axis %s — no scenario carries FML traffic (reads or writes); map manually if you know better\n", f.Entry, axis)
			} else {
				aiNames := aiNameScenarios(ctx, log, client, bd, f, scens, diff, src, rec)
				draft = renderScenarioDraft(f, axis, scens, diff, aiNames, dirMode, tree, axes, svc)
			}
			if !stdout {
				scenDir := filepath.Join(filepath.Dir(out), config.DefaultScenDir)
				paths, aerr := scenarioArtifacts(log, scenDir, f.Entry, src, scens, f, tree)
				if aerr != nil {
					return written, aerr
				}
				axesPaths, aerr := axesArtifacts(log, scenDir, f.Entry, axes)
				if aerr != nil {
					return written, aerr
				}
				paths = append(paths, axesPaths...)
				for _, p := range paths {
					fmt.Printf("  artifact: %s\n", p)
				}
			}
			if draft == "" {
				continue
			}
		} else {
			candidates := flow.Discover(tree, f.Conditions)
			if len(candidates) == 0 {
				fmt.Printf("- %s: no API candidates found (%d conditions inspected, no dispatch axis)\n", f.Entry, len(f.Conditions))
				continue
			}
			fmt.Printf("- %s: no dispatch axis — condition-based discovery\n", f.Entry)
			printReturnCrossCheck(tree, facts, f)
			aiNames := aiNameEndpoints(ctx, log, client, bd, f, tree, candidates, src, rec)
			draft = renderDraft(f, candidates, dirMode, aiNames, svc)
			printDiscoverSummary(f, tree, candidates)
		}
		if stdout {
			fmt.Println()
			fmt.Println(draft)
			continue
		}
		base := strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path))
		path, kept, err := writeDraft(out, base+".mapping.yaml", draft)
		if err != nil {
			return written, err
		}
		if kept {
			existing++
		}
		written++
		log.Info("draft written", "path", path)
		fmt.Printf("  draft: %s\n", path)
	}
	if stdout {
		return written, nil
	}
	fmt.Printf("\n%d draft(s) written to %s (%d existing kept) — review name/route, fill module/readDBs if you generate into an existing repo, then: tuxconv convert %s\n",
		written, out, existing, target)
	return written, nil
}

// writeDraft persists one draft yaml, never clobbering an existing draft —
// a tagged draft is user work, so the fresh draft lands alongside as a
// numbered sibling ("<name> (1).mapping.yaml", first free number). Returns
// the written path and whether an existing draft was kept.
func writeDraft(out, name, draft string) (string, bool, error) {
	path := filepath.Join(out, name)
	kept := false
	if _, err := os.Stat(path); err == nil {
		kept = true
		fmt.Printf("  note: %s kept — writing a fresh draft alongside\n", path)
		for i := 1; ; i++ {
			cand := filepath.Join(out, strings.Replace(name, ".mapping.yaml", fmt.Sprintf(" (%d).mapping.yaml", i), 1))
			if _, err := os.Stat(cand); os.IsNotExist(err) {
				path = cand
				break
			}
		}
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return path, kept, err
	}
	if err := os.WriteFile(path, []byte(draft), 0o644); err != nil {
		return path, kept, fmt.Errorf("discover: write %s: %w", path, err)
	}
	return path, kept, nil
}

// serviceName derives the draft's default service name (the generated
// package/directory key) from the entry file stem: lower snake with
// non-identifier bytes folded to '_' and a leading digit escaped, so the
// emitted draft always passes the loader's identifier check.
func serviceName(f *ir.File) string {
	stem := strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path))
	return common.EscapeLeadingDigit(common.Snake(stem))
}

// uniqueServiceName keeps service names unique across one discover run and
// the drafts already on disk: the first occurrence keeps the stem-derived
// base, a collision gains the first free numeric suffix ("item", "item2", …).
// The service name keys the generated package/directory, so two drafts
// under one service would share a generation target.
func uniqueServiceName(base string, used map[string]bool) string {
	if !used[base] {
		return base
	}
	for i := 2; ; i++ {
		if cand := base + strconv.Itoa(i); !used[cand] {
			return cand
		}
	}
}

// usedServiceNames seeds the run's service-name uniqueness space from the
// Go mapping drafts already in out (never-clobber accumulates them there).
// A draft that fails to parse contributes nothing — discover must not fail
// on a file it merely wants to avoid repeating.
func usedServiceNames(out string) map[string]bool {
	used := map[string]bool{}
	entries, err := os.ReadDir(out)
	if err != nil {
		return used
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".mapping.yaml") || strings.HasSuffix(name, ".cs.mapping.yaml") {
			continue
		}
		if svc, err := plan.MappingServiceOf(filepath.Join(out, name)); err == nil && svc != "" {
			used[svc] = true
		}
	}
	return used
}

// discoverOutDir resolves the draft directory: the -out override, else the
// mappings/ convention next to the working directory.
func discoverOutDir(flagOut string) string {
	if flagOut != "" {
		return flagOut
	}
	return config.DefaultMappingsDir
}

// renderDraft emits the mapping draft (DIS-D5): only strict-decodable keys,
// metadata in comments, pre-filled name/route (AI- or deterministic-suggested,
// always editable), census comments, dbMethods pins when the AI proposed
// them, and the non-candidates kept (commented) for user control. Module and
// readDBs stay commented hints — module defaults to the service name at
// load, so a draft is loadable as-is once names exist. service is discover's
// run-unique name; empty derives the default from the entry stem.
func renderDraft(f *ir.File, candidates []flow.Candidate, dirMode bool, aiNames map[string]aiSuggestion, service string) string {
	entry := filepath.Base(f.Path)
	svc := service
	if svc == "" {
		svc = serviceName(f)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s — generated by tuxconv (endpoint discovery).\n", entry)
	sb.WriteString("#\n")
	sb.WriteString("# HOW TO USE: names/routes below are suggested defaults — review them\n")
	sb.WriteString("# (or delete entries you do not want), optionally set module/readDBs,\n")
	sb.WriteString("# then convertgo. A condition you omit stays logic-only. `condition:` is\n")
	sb.WriteString("# the top-level inventory index; `conditionRef:` is a nested block.\n")
	sb.WriteString("\n")
	if dirMode {
		fmt.Fprintf(&sb, "source: %s\n", entry)
	}
	fmt.Fprintf(&sb, "service: %s\n", svc)
	fmt.Fprintf(&sb, "# module: %s   # defaults to the service name; set your real import path for an existing repo\n", svc)
	sb.WriteString("# readDBs:                     # logical read-DB names for handler wiring\n")
	sb.WriteString("#   - EXAMPLE\n")
	sb.WriteString("\nendpoints:\n")
	mapped := map[int]string{} // condition index → candidate key ("" when only non-candidate)
	for _, c := range candidates {
		n := strings.SplitN(strings.TrimPrefix(c.Key, "c"), ".", 2)[0]
		var idx int
		fmt.Sscanf(n, "%d", &idx)
		if _, ok := mapped[idx]; !ok {
			mapped[idx] = c.Key
		}
	}
	for _, c := range candidates {
		if strings.Contains(c.Key, ".") {
			fmt.Fprintf(&sb, "  - conditionRef: %-12s # nested block, lines %d-%d | reads: %s | writes: %s",
				c.Key, c.StartLine, c.EndLine,
				fieldsOrDash(c.Gets), fieldsOrDash(c.Adds))
			if len(c.QueryIDs) > 0 {
				fmt.Fprintf(&sb, " | queries: %s", strings.Join(c.QueryIDs, ","))
			}
			if len(c.Codes) > 0 {
				fmt.Fprintf(&sb, " | error codes: %s", strings.Join(c.Codes, ","))
			}
			if c.Redundant {
				sb.WriteString(" | NOTE: subset of its parent — consider tagging the parent")
			}
			sb.WriteString("\n")
		} else {
			fmt.Fprintf(&sb, "  - condition: %-15s # lines %d-%d | reads: %s | writes: %s",
				strings.TrimPrefix(c.Key, "c"), c.StartLine, c.EndLine,
				fieldsOrDash(c.Gets), fieldsOrDash(c.Adds))
			if len(c.QueryIDs) > 0 {
				fmt.Fprintf(&sb, " | queries: %s", strings.Join(c.QueryIDs, ","))
			}
			if len(c.Codes) > 0 {
				fmt.Fprintf(&sb, " | error codes: %s", strings.Join(c.Codes, ","))
			}
			sb.WriteString("\n")
		}
		sug := aiNames[c.Key]
		origin := "# TODO tag the Go method name"
		switch {
		case sug.Name == "":
		case sug.Deterministic:
			origin = "# deterministic — edit freely"
		default:
			origin = "# ai-suggested — edit freely"
		}
		fmt.Fprintf(&sb, "    name: %-16s %s\n", strconv.Quote(sug.Name), origin)
		switch {
		case sug.Route == "":
			sb.WriteString("    route: \"\"                 # TODO tag the route path (starts with /)\n")
		case sug.Deterministic:
			fmt.Fprintf(&sb, "    route: %-15s # deterministic — edit freely\n", strconv.Quote(sug.Route))
		default:
			fmt.Fprintf(&sb, "    route: %-15s # ai-suggested — edit freely\n", strconv.Quote(sug.Route))
		}
	}
	// Non-candidates, commented, for control.
	var rest []int
	for _, cond := range f.Conditions {
		if _, ok := mapped[cond.Index]; !ok {
			rest = append(rest, cond.Index)
		}
	}
	if len(rest) > 0 {
		sb.WriteString("# not API candidates by the discovery rubric — map manually if you know better:\n")
		for _, idx := range rest {
			cond := &f.Conditions[idx-1]
			gets, adds, _ := flow.Census(cond)
			fmt.Fprintf(&sb, "# - condition: %-13d # lines %d-%d | reads: %s | writes: %s\n",
				idx, cond.StartLine, cond.EndLine, fieldsOrDash(gets), fieldsOrDash(adds))
		}
	}
	// dbMethods: every unique query's method name pre-filled — the exact
	// name the plan derives when the mapping leaves it unpinned. AI-proposed
	// pins override where the AI proposed them; a user edit wins over both.
	pins := map[string]methodPinSuggestion{}
	for _, sug := range aiNames {
		for id, m := range sug.Methods {
			if _, dup := pins[id]; !dup {
				pins[id] = m
			}
		}
	}
	writeDBMethods(&sb, f, pins)
	return sb.String()
}

// writeDBMethods emits the draft's dbMethods block with every unique
// query's method name pre-filled (plan.DefaultMethodNames — the draft and
// the plan's fallback cannot drift). AI-proposed pins override where the
// AI proposed them; params/row stay derived from the query binds.
func writeDBMethods(sb *strings.Builder, f *ir.File, aiPins map[string]methodPinSuggestion) {
	names := plan.DefaultMethodNames(f.Queries)
	if len(names) == 0 {
		return
	}
	sb.WriteString("\ndbMethods:                     # deterministic — edit freely; params stay\n")
	sb.WriteString("                               # derived from the query binds\n")
	ids := make([]string, 0, len(names))
	for id := range names {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p, ai := aiPins[id]
		switch {
		case ai && p.Name != "" && p.Row != "":
			fmt.Fprintf(sb, "  %s: {name: %s, row: %s}   # ai-suggested — edit freely\n", id, p.Name, p.Row)
		case ai && p.Name != "":
			fmt.Fprintf(sb, "  %s: {name: %s}   # ai-suggested — edit freely\n", id, p.Name)
		default:
			fmt.Fprintf(sb, "  %-26s # deterministic — edit freely\n", id+": {name: "+names[id]+"}")
		}
	}
	sb.WriteString("# `fn_<name>:<qid>` keys pin queries inside an external fn library file.\n")
}

// renderScenarioDraft emits the mapping draft for an axis entry (SCEN-5):
// one scenarioRef endpoint per qualifying scenario (the census rubric —
// request reads AND non-error response writes), census comments carrying
// the tx evidence, pre-filled advisory names, the non-qualifying scenarios
// kept (commented) for user control, and registry-derived scenarioFilter
// examples (commented) for merged slices. Module and readDBs stay commented
// hints — module defaults to the service name at load. service is
// discover's run-unique name; empty derives the default from the entry stem.
func renderScenarioDraft(f *ir.File, axis *flow.DispatchAxis, scens []*flow.Scenario, diff *flow.ScenarioDiff, aiNames map[string]aiSuggestion, dirMode bool, tree *flow.Tree, axes []*flow.DispatchAxis, service string) string {
	entry := filepath.Base(f.Path)
	svc := service
	if svc == "" {
		svc = serviceName(f)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s — generated by tuxconv (dispatch-axis scenarios).\n", entry)
	sb.WriteString("#\n")
	sb.WriteString("# HOW TO USE: names/routes below are suggested defaults — review them\n")
	sb.WriteString("# (or delete entries you do not want), optionally set module/readDBs,\n")
	sb.WriteString("# then convertgo. The entry folds into one scenario slice per value of its\n")
	sb.WriteString("# dispatch axis, plus the default arm when the dispatch chain ends in an\n")
	sb.WriteString("# else (`<var>=default`); every reachable arm gets exactly one slice.\n")
	sb.WriteString("# `scenarioRef:` names one slice (`<var>=<value>`). A scenario you omit\n")
	sb.WriteString("# stays logic-only. Shared/unique-block evidence:\n")
	sb.WriteString("# scenarios/<entry>.shared.md.\n")
	sb.WriteString("\n")
	if dirMode {
		fmt.Fprintf(&sb, "source: %s\n", entry)
	}
	fmt.Fprintf(&sb, "service: %s\n", svc)
	fmt.Fprintf(&sb, "# module: %s   # defaults to the service name; set your real import path for an existing repo\n", svc)
	sb.WriteString("# readDBs:                     # logical read-DB names for handler wiring\n")
	sb.WriteString("#   - EXAMPLE\n")
	sb.WriteString("\nendpoints:\n")
	var logicOnly []*flow.Scenario
	for _, sc := range scens {
		if !sc.CarriesContract() {
			logicOnly = append(logicOnly, sc)
			continue
		}
		ext := sc.BodyExtent()
		fmt.Fprintf(&sb, "  - scenarioRef: %-16s # kept lines %d-%d | reads: %s | writes: %s",
			scenarioRefValue(sc), ext[0], ext[1], censusList(sc.Gets, 12), censusList(sc.Adds, 12))
		if len(sc.Adds) == 0 {
			sb.WriteString(" | message-only: handler returns the message string")
		}
		if qids := scenarioQueryIDs(sc); len(qids) > 0 {
			fmt.Fprintf(&sb, " | queries: %d (%s)", len(qids), censusList(qids, 20))
		}
		if txIDs := scenarioTxIDs(sc); len(txIDs) > 0 {
			fmt.Fprintf(&sb, " | tx: %s", strings.Join(txIDs, ","))
		}
		if len(sc.Codes) > 0 {
			fmt.Fprintf(&sb, " | error codes: %s", censusList(sc.Codes, 12))
		}
		sb.WriteString("\n")
		writeSuggestion(&sb, aiNames[sc.Key])
	}
	if len(logicOnly) > 0 {
		sb.WriteString("# scenarios with no FML traffic (pure logic) — map manually if you know better:\n")
		for _, sc := range logicOnly {
			fmt.Fprintf(&sb, "# - scenarioRef: %-14s # reads: %s | writes: %s | queries: %d\n",
				scenarioRefValue(sc), fieldsOrDash(sc.Gets), fieldsOrDash(sc.Adds), len(scenarioQueryIDs(sc)))
		}
	}
	writeFilterExamples(&sb, tree, axes)
	// dbMethods: the same pre-filled block as the candidate draft — AI
	// proposals override where the AI offered them.
	pins := map[string]methodPinSuggestion{}
	for _, sug := range aiNames {
		for id, m := range sug.Methods {
			if _, dup := pins[id]; !dup {
				pins[id] = m
			}
		}
	}
	writeDBMethods(&sb, f, pins)
	return sb.String()
}

// scenarioRefValue renders the draft's scenarioRef value ("trn_cd=A").
func scenarioRefValue(sc *flow.Scenario) string {
	return sc.Var + "=" + sc.Value
}

// scenarioTxIDs lists the scenario's tx-flagged DML query ids.
func scenarioTxIDs(sc *flow.Scenario) []string {
	var out []string
	for _, q := range sc.Queries {
		if q.DML && q.Tx {
			out = append(out, q.ID)
		}
	}
	return out
}

// writeSuggestion renders one endpoint's name/route lines with the
// advisory-origin comment.
func writeSuggestion(sb *strings.Builder, sug aiSuggestion) {
	origin := "# TODO tag the Go method name"
	switch {
	case sug.Name == "":
	case sug.Deterministic:
		origin = "# deterministic — edit freely"
	default:
		origin = "# ai-suggested — edit freely"
	}
	fmt.Fprintf(sb, "    name: %-16s %s\n", strconv.Quote(sug.Name), origin)
	switch {
	case sug.Route == "":
		sb.WriteString("    route: \"\"                 # TODO tag the route path (starts with /)\n")
	case sug.Deterministic:
		fmt.Fprintf(sb, "    route: %-15s # deterministic — edit freely\n", strconv.Quote(sug.Route))
	default:
		fmt.Fprintf(sb, "    route: %-15s # ai-suggested — edit freely\n", strconv.Quote(sug.Route))
	}
}

// printReturnCrossCheck is the return-anchored II validation line: every
// success terminal (tpreturn(TPSUCCESS)/tpforward) is an outcome the
// condition/axis split must account for. It reports in-branch outcomes vs
// convergent tail returns (whose arms carry the split) and forwarded
// delegations — advisory only, never changes the draft.
func printReturnCrossCheck(tree *flow.Tree, facts *tsscan.SourceFacts, f *ir.File) {
	if tree == nil || facts == nil || f == nil || f.Entry == "" {
		return
	}
	outs := flow.DiscoverByReturn(tree, f.Conditions, facts, f.Entry)
	if len(outs) == 0 {
		return
	}
	inBranch, conv, forwards := 0, 0, []string{}
	for _, o := range outs {
		switch {
		case o.Kind == "forward":
			forwards = append(forwards, fmt.Sprintf("L%d→%s", o.ReturnLine, o.ForwardTo))
		case o.Convergent:
			conv++
		default:
			inBranch++
		}
	}
	fmt.Printf("  outcomes: %d success terminal(s) — %d in-branch, %d convergent tail(s)",
		len(outs), inBranch, conv)
	if len(forwards) > 0 {
		fmt.Printf(", %d forward(s) [%s]", len(forwards), strings.Join(forwards, ", "))
	}
	fmt.Println()
	for _, o := range outs {
		if o.Convergent {
			fmt.Printf("    tail L%d: %d arm(s) feed it — split inside by arm\n", o.ReturnLine, len(o.Arms))
		}
	}
}

func printDiscoverSummary(f *ir.File, tree *flow.Tree, candidates []flow.Candidate) {
	top, nested := 0, 0
	for _, c := range candidates {
		if strings.Contains(c.Key, ".") {
			nested++
		} else {
			top++
		}
	}
	fmt.Printf("\n%s: %d candidate(s) (%d top-level, %d nested) from %d condition(s), coverage %d/%d lines\n",
		f.Entry, len(candidates), top, nested, len(f.Conditions),
		tree.Coverage.Classified, tree.Coverage.CodeLines)
	for _, c := range candidates {
		note := ""
		if c.Redundant {
			note = "  (redundant with parent)"
		}
		fmt.Printf("  %-8s lines %d-%d  reads %-2d writes %-2d%s\n",
			c.Key, c.StartLine, c.EndLine, len(c.Gets), len(c.Adds), note)
	}
}

// printScenarioSummary is the axis path's per-entry scan report (SCEN-5):
// the detected axis, every scenario's census shape, and which qualify.
func printScenarioSummary(f *ir.File, axis *flow.DispatchAxis, scens []*flow.Scenario) {
	fmt.Printf("\n%s: %d scenario(s) on %s\n", f.Entry, len(scens), axis)
	for _, sc := range scens {
		note := ""
		switch {
		case len(sc.Gets) == 0 && len(sc.Adds) == 0:
			note = "  (logic-only — no FML traffic)"
		case len(sc.Adds) == 0:
			note = "  (message-only — handler returns the message string)"
		case len(sc.Gets) == 0:
			note = "  (emit-only — empty request body)"
		}
		fmt.Printf("  %-12s kept %3d dropped %3d unfolded %d  reads %-2d writes %-2d queries %-3d%s\n",
			scenarioRefValue(sc), sc.Counts.Kept, sc.Counts.Dropped, sc.Counts.Unfolded,
			len(sc.Gets), len(sc.Adds), len(scenarioQueryIDs(sc)), note)
	}
}

func fieldsOrDash(fields []string) string {
	if len(fields) == 0 {
		return "none"
	}
	sorted := append([]string(nil), fields...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

// censusList renders a draft-comment census list compactly: the first cap
// entries, then "+N more" (the full census lives in the scenario artifacts
// and the shared report).
func censusList(fields []string, cap int) string {
	if len(fields) == 0 {
		return "none"
	}
	if len(fields) <= cap {
		return strings.Join(fields, ",")
	}
	return strings.Join(fields[:cap], ",") + fmt.Sprintf(" +%d more", len(fields)-cap)
}
