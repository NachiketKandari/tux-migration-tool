package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"tux-migration-tool/internal/audit"
	"tux-migration-tool/internal/config"
	"tux-migration-tool/internal/inline"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/llm"
	"tux-migration-tool/internal/plan"
	"tux-migration-tool/internal/telemetry"
)

const defaultConfigFile = ".tuxconv.yaml"

// legacyConfigFiles are the pre-sanitization / alternate spellings still
// honored as local-file fallbacks (in order) so existing working copies
// keep working without a rename.
var legacyConfigFiles = []string{".tuxconvert.yaml", ".tuxgo.yaml"}

// configEnvVar names the server-level config pointer: when no -config flag
// and no ./.tuxconv.yaml resolve, the file at this path supplies the run
// configuration (models[].apiKey / database.dsn literals included). The web
// viewer sets it once for the server process; every tuxconv call the viewer
// spawns then shares the CLI's yaml keys instead of the stock defaults.
const configEnvVar = "TUXCONV_CONFIG"

// legacyConfigEnvVars are honored in order after configEnvVar.
var legacyConfigEnvVars = []string{"TUXCONVERT_CONFIG", "TUXGO_CONFIG"}

// runExtract implements `tuxconv extract <file|dir>` — the Phase 2 IR
// extraction + marking flow (deterministic, zero LLM calls). The config is
// loaded and its model profile routed so every run exercises the same
// config seam the generation phases will use; extraction itself never
// touches the LLM.
func runExtract(ctx context.Context, args []string) error {
	log := telemetry.Log(ctx)
	fs := flag.NewFlagSet("extract", flag.ContinueOnError)
	outPath := fs.String("out", "", "Write the IR JSON to this path (single-file mode defaults to stdout)")
	configPath := fs.String("config", "", "Path to .tuxconv.yaml (default: ./.tuxconv.yaml when present, else defaults)")
	fragment := fs.Bool("fragment", false, "Force fragment mode on a single file (PF-3.1: detection is otherwise automatic)")
	noInline := fs.Bool("no-inline-fns", false, "Do not materialize cross-file fn_* helpers into their caller (directory mode only; they keep the legacy stub/TODO handling)")

	flagArgs, positional := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(positional) == 0 {
		return fmt.Errorf("must provide a .pc/.pcf file or a directory to extract")
	}
	target := positional[0]

	cfg, cfgSource, err := loadRunConfig(*configPath)
	if err != nil {
		return err
	}
	logConfigRouting(ctx, cfg, cfgSource)
	irOpts := irOptions(cfg, *fragment)

	fi, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("cannot access target path %s: %w", target, err)
	}

	if fi.IsDir() {
		files, err := ir.ExtractDirOpts(target, irOpts)
		if err != nil {
			return err
		}
		if len(files) == 0 {
			fmt.Fprintf(os.Stderr, "no .pc or .pcf files found in %s\n", target)
			return nil
		}
		// Cross-file fn helpers are materialized into their caller before
		// the IR is written, so the archive the user inspects is the IR the
		// conversion actually runs on (see inline.go).
		expanded, results, err := expandCorpusFns(ctx, files, *noInline)
		if err != nil {
			return err
		}
		for _, f := range files {
			if e, ok := expanded[f.Path]; ok {
				*f = *e
			}
		}
		return writeDirIR(ctx, cfg, *outPath, files, results)
	}

	file, err := ir.ExtractFileOpts(target, irOpts)
	if err != nil {
		return err
	}
	logFileIR(ctx, file)
	if err := archiveIR(ctx, file, "ir-"+strings.TrimSuffix(filepath.Base(file.Path), filepath.Ext(file.Path))+".json"); err != nil {
		log.Warn("audit archive unavailable", "error", err)
	}

	data, err := marshalIR(file)
	if err != nil {
		return err
	}
	if *outPath == "" {
		fmt.Println(string(data))
	} else {
		if err := os.WriteFile(*outPath, data, 0o644); err != nil {
			return fmt.Errorf("failed writing IR to %s: %w", *outPath, err)
		}
		log.Info("ir written", "path", *outPath, "queries", len(file.Queries), "conditions", len(file.Conditions))
	}
	log.Info("extraction complete", "file", file.Path, "queries", len(file.Queries), "unique_queries", len(file.UniqueQueries()), "conditions", len(file.Conditions))
	return nil
}

// irOptions maps the run config onto extraction options (PF-4.1 buffer
// registry, PF-3.1 forced fragment mode).
func irOptions(cfg *config.Config, forceFragment bool) ir.Options {
	return ir.Options{BufferRoles: cfg.Buffers.Roles, ForceFragment: forceFragment}
}

// loadRunConfig resolves the run configuration: explicit -config path, else
// ./.tuxconv.yaml when present in the working directory (then the legacy
// .tuxconvert.yaml / .tuxgo.yaml spellings, in order), else the file named
// by $TUXCONV_CONFIG (then $TUXCONVERT_CONFIG / $TUXGO_CONFIG) — the
// server-level config: the web viewer runs tuxconv
// inside disposable job tmpdirs that carry no .tuxconv.yaml, so the operator
// points TUXCONV_CONFIG at one yaml whose models[].apiKey / database.dsn
// literals the browser runs then share with the CLI), else the stock
// defaults. A config-env pointer that names a missing file is a loud error —
// a typo'd pointer must never silently run keyless.
func loadRunConfig(explicit string) (*config.Config, string, error) {
	path := explicit
	if path == "" {
		for _, candidate := range append([]string{defaultConfigFile}, legacyConfigFiles...) {
			if _, err := os.Stat(candidate); err == nil {
				path = candidate
				break
			}
		}
	}
	if path == "" {
		for _, envVar := range append([]string{configEnvVar}, legacyConfigEnvVars...) {
			if env := strings.TrimSpace(os.Getenv(envVar)); env != "" {
				path = env
				break
			}
		}
	}
	if path == "" {
		return config.Default(), "defaults", nil
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, "", err
	}
	return cfg, path, nil
}

// resolveInput picks the conversion target: the CLI positional wins, else
// convert.input from the yaml, else an error naming both sources.
func resolveInput(positional []string, cfg *config.Config) (string, error) {
	if len(positional) > 0 {
		return positional[0], nil
	}
	if cfg.Convert.Input != "" {
		return cfg.Convert.Input, nil
	}
	return "", fmt.Errorf("no input target: pass a .pc/.pcf file or directory, or set convert.input in .tuxconv.yaml")
}

// resolveBatchInput picks the batchpy conversion target: the CLI positional
// wins, else batchpy.input from the yaml, else an error naming both sources.
func resolveBatchInput(positional []string, cfg *config.Config) (string, error) {
	if len(positional) > 0 {
		return positional[0], nil
	}
	if cfg.Batchpy.Input != "" {
		return cfg.Batchpy.Input, nil
	}
	return "", fmt.Errorf("no input target: pass a .pc/.pcf file or directory, or set batchpy.input in .tuxconv.yaml")
}

// defaultMappingsDir is the mapping-draft convention: discover and
// convert's no-mapping fallback write here; convert resolves it last.
const defaultMappingsDir = config.DefaultMappingsDir

// mappingFor resolves the endpoint mapping: the -mapping flag wins, else
// convert.mapping from the yaml, else the mappings/ convention when that
// directory holds at least one draft yaml. "" = nothing found (convert
// then drafts; plan errors).
func mappingFor(flagValue string, cfg *config.Config) string {
	if flagValue != "" {
		return flagValue
	}
	if cfg.Convert.Mapping != "" {
		return cfg.Convert.Mapping
	}
	if fi, err := os.Stat(defaultMappingsDir); err == nil && fi.IsDir() && hasMappingYamls(defaultMappingsDir) {
		return defaultMappingsDir
	}
	return ""
}

// hasMappingYamls reports whether dir holds at least one .yaml/.yml file.
func hasMappingYamls(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".yaml", ".yml":
			return true
		}
	}
	return false
}

// mappingForEntry picks one mapping yaml out of a directory for a
// single-entry convert: a mapping whose source: names the entry file wins,
// else the one whose stem matches the entry stem. Matching is lenient —
// only the source field is read, so unrelated drafts in the shared
// convention dir never poison the run — and only the winner is validated
// (by the caller's LoadMapping). Zero matches returns "" so the caller
// drafts-and-stops; two stem/source matches refuse to guess.
func mappingForEntry(dir, entryBase string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("convert: read mapping dir %s: %w", dir, err)
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
		src, err := plan.MappingSourceOf(path)
		if err != nil {
			return "", err
		}
		if src != "" && strings.EqualFold(filepath.Base(src), entryBase) {
			bySource = append(bySource, path)
		}
		// Stem match tolerates the .mapping.yaml double extension the
		// draft convention uses (SVC_X.mapping.yaml ↔ SVC_X.pc).
		nameStem := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		nameStem = strings.TrimSuffix(nameStem, ".mapping")
		if nameStem == stemOf(entryBase) {
			byStem = append(byStem, path)
		}
	}
	switch {
	case len(bySource) == 1:
		return bySource[0], nil
	case len(bySource) > 1:
		return "", fmt.Errorf("convert: mappings %s and %s both declare source %s — pass -mapping <file> explicitly", bySource[0], bySource[1], entryBase)
	case len(byStem) == 1:
		return byStem[0], nil
	case len(byStem) > 1:
		return "", fmt.Errorf("convert: %s holds several mappings for %s — pass -mapping <file> explicitly", dir, entryBase)
	default:
		return "", nil
	}
}

// stemOf strips the source extension (case-insensitive) so the stem match
// lines up with the draft convention, which names drafts after the source
// stem: SVC_X.pc → SVC_X, SVC_X.pcf → SVC_X, and a plain C service too
// (SVC_X.c → SVC_X). An extensionless name rides along untouched.
//
// Restricting this to .pc/.pcf stranded every other source shape: the draft
// landed as <stem>.mapping.yaml while the lookup compared against the
// unextended name, matched nothing, and the second pass re-drafted forever.
func stemOf(name string) string {
	if ext := filepath.Ext(name); ext != "" {
		return strings.TrimSuffix(name, ext)
	}
	return name
}

// resolveLLMClient is the one AI knob, shared by every LLM-bound command
// (convert, batchpy, gentest, discover): the client exists only when
// run.llm is true and -no-llm was not passed. A nil client means "run
// deterministic" for the caller's seam; the availability WARN keeps the
// degrade visible. Never log the key value itself.
func resolveLLMClient(ctx context.Context, cfg *config.Config, noLLM bool, purpose string) llm.Client {
	log := telemetry.Log(ctx)
	if !cfg.Run.LLM || noLLM {
		log.Info("llm disabled — deterministic-only run", "purpose", purpose)
		return nil
	}
	c, err := llm.NewFromConfig(ctx, cfg, "")
	if err != nil {
		log.Warn("llm client unavailable — falling back to deterministic behavior", "purpose", purpose, "error", err)
		return nil
	}
	return c
}

// logConfigRouting proves the yaml routing seam end to end: which profile
// the run routes to, its endpoint, and where the key came from — never the
// key value itself.
func logConfigRouting(ctx context.Context, cfg *config.Config, source string) {
	log := telemetry.Log(ctx)
	m, err := cfg.Route("")
	if err != nil {
		log.Warn("config routing failed", "error", err, "config", source)
		return
	}
	_, keySource, keyErr := m.ResolveKey()
	if keyErr != nil {
		keySource = "unresolved"
	}
	log.Info("config routed",
		"config", source,
		"profile", m.Name,
		"model", m.Model,
		"api_base", m.APIBase,
		"key_source", keySource,
	)
}

func writeDirIR(ctx context.Context, cfg *config.Config, outPath string, files []*ir.File, inlineResults map[string]*inline.Result) error {
	log := telemetry.Log(ctx)
	// Flag > config (C1): -out redirects the directory-mode IR state dir —
	// pre-fix the flag was silently ignored here (engine-wiring audit
	// Tier-1 #12).
	stateDir := outPath
	if stateDir == "" {
		stateDir = cfg.Paths.State
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("failed creating IR state dir %s: %w", stateDir, err)
	}

	for _, file := range files {
		logFileIR(ctx, file)
		base := strings.TrimSuffix(filepath.Base(file.Path), filepath.Ext(file.Path))
		path := filepath.Join(stateDir, base+".ir.json")
		data, err := marshalIR(file)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("failed writing IR to %s: %w", path, err)
		}
		if err := archiveIR(ctx, file, "ir-"+base+".json"); err != nil {
			log.Warn("audit archive unavailable", "error", err)
		}

		unresolved := 0
		for _, ext := range file.ExternalFns {
			if !ext.Resolved {
				unresolved++
			}
		}
		fmt.Printf("%s: entry=%s conditions=%d queries=%d unique=%d external_fns=%d unresolved=%d unbalanced=%d%s → %s\n",
			filepath.Base(file.Path), orDash(file.Entry), len(file.Conditions),
			len(file.Queries), len(file.UniqueQueries()), len(file.ExternalFns), unresolved, len(file.Unbalanced),
			inlineSummary(inlineResults[file.Path]), path)
	}
	log.Info("extraction complete", "files", len(files), "state_dir", stateDir)
	return nil
}

// logFileIR emits the per-unit extraction trace (architecture.md §4 schema).
func logFileIR(ctx context.Context, file *ir.File) {
	log := telemetry.Log(ctx)
	for _, q := range file.Queries {
		log.Info("extracted query unit",
			"file", file.Path,
			"query_id", q.ID,
			"query_type", string(q.Type),
			"template_id", q.TemplateID,
			"table", strings.Join(q.Tables, ","),
			"binds", len(q.Binds),
			"cursor_flattened", q.CursorFlattened,
			"duplicate_of", q.DuplicateOf,
			"lines", fmt.Sprintf("%d-%d", q.StartLine, q.EndLine),
		)
	}
	for _, ext := range file.ExternalFns {
		if ext.Resolved {
			log.Info("external fn resolved", "file", file.Path, "fn", ext.Name, "defined_in", ext.DefinedIn, "queries", strings.Join(ext.QueryIDs, ","))
		} else {
			log.Warn("external fn unresolved — generation blocks pending its defining file", "file", file.Path, "fn", ext.Name)
		}
	}
	for _, u := range file.Unbalanced {
		log.Warn("unbalanced region — the parse continues leniently past it, so downstream facts may be truncated",
			"file", file.Path, "kind", u.Kind, "line", u.Line, "col", u.Col)
	}
	for _, pe := range file.ParseErrors {
		log.Warn("parse error — the grammar recovered, but downstream facts over this region are untrusted",
			"file", file.Path, "kind", pe.Kind, "line", pe.Line, "col", pe.Col)
	}
	for _, tp := range file.TPCalls {
		if tp.ServiceFile != "" {
			log.Info("tpcall site resolved", "file", file.Path, "service", tp.Service,
				"service_file", tp.ServiceFile, "lines", fmt.Sprintf("%d-%d", tp.StartLine, tp.EndLine))
		} else {
			log.Warn("tpcall site — target service outside the scanned corpus",
				"file", file.Path, "service", tp.Service, "lines", fmt.Sprintf("%d-%d", tp.StartLine, tp.EndLine))
		}
	}
	log.Info("file extracted",
		"file", file.Path,
		"entry", orDash(file.Entry),
		"conditions", len(file.Conditions),
		"queries", len(file.Queries),
		"host_vars", len(file.HostVars),
		"unbalanced", len(file.Unbalanced),
		"parse_errors", len(file.ParseErrors),
	)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func marshalIR(file *ir.File) ([]byte, error) {
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed serializing IR: %w", err)
	}
	return data, nil
}

// archiveIR persists one file's IR into the run's audit folder (§4.7).
func archiveIR(ctx context.Context, file *ir.File, name string) error {
	rec, err := audit.New(auditDir, telemetry.RunIDFromContext(ctx))
	if err != nil {
		return err
	}
	path, err := rec.WriteJSON(name, file)
	if err != nil {
		return err
	}
	telemetry.Log(ctx).Info("ir archived", "path", path)
	return nil
}
