// FN inlining, CLI seam (user directive 2026-09-30).
//
// A service that calls an fn_* helper defined in another .pc file used to
// leave that helper as an opaque ExternalFn: the plan stubbed it or dropped
// it, and flow/render emitted "TODO external fn call (plan resolves)" at the
// call site, so the body was never converted. internal/inline resolves the
// helper against the scanned corpus and materializes its definition into the
// caller, which makes it a locally-defined helper — the shape plan.Build
// already turns into a KindFnHelper unit (controller/fns.go) that the
// generated controller calls.
//
// This file is the ONE place that decides whether the pass runs, so extract,
// plan, and convertgo cannot drift apart: the IR a user inspects is the IR
// the conversion uses. Two rules govern the wiring:
//
//   - Directory mode only. A single-file run has no corpus, so there is
//     nothing to resolve against; it is untouched.
//   - The pass is a strict no-op for a file with no external fns, so a
//     corpus that never needed inlining produces byte-identical output.
//
// Every refusal (chk_* session plumbing, an unresolved helper, a helper that
// touches the caller's FML buffers) is reported, never silent — the repo's
// standing rule, and a stub the user cannot see is worse than a slow run.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"

	"tux-migration-tool/internal/inline"
	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/telemetry"
)

// corpusSources reads the source text for every extracted file. The inline
// pass needs the callee's bytes to lift a definition, and reading them once
// here keeps every caller from re-reading the corpus.
func corpusSources(files []*ir.File) (map[string]string, error) {
	out := make(map[string]string, len(files))
	for _, f := range files {
		if f == nil {
			continue
		}
		if _, ok := out[f.Path]; ok {
			continue
		}
		b, err := os.ReadFile(f.Path)
		if err != nil {
			return nil, fmt.Errorf("inline: read %s: %w", f.Path, err)
		}
		out[f.Path] = string(b)
	}
	return out, nil
}

// expandFns runs the inline pass for one main file over a scanned corpus and
// returns the IR and source the conversion should use.
//
// main and source are returned UNCHANGED when the pass is disabled, when the
// target was a single file, or when there was nothing to inline — so the
// opt-out is byte-exact, not approximately so.
//
// sources lets a caller that serves SEVERAL mains over one corpus read the
// corpus once (see corpusSources). Pass nil to read it here.
func expandFns(ctx context.Context, main *ir.File, files []*ir.File, source string, disabled bool, sources map[string]string) (*ir.File, string, error) {
	if disabled || main == nil || len(files) < 2 {
		return main, source, nil
	}
	if len(main.ExternalFns) == 0 {
		return main, source, nil
	}
	if sources == nil {
		var err error
		sources, err = corpusSources(files)
		if err != nil {
			return nil, "", err
		}
	}
	res, err := inline.Expand(main, inline.NewCorpus(files, sources), inline.Options{
		IROptions: ir.DefaultOptions(),
	})
	if err != nil {
		return nil, "", err
	}
	reportInline(ctx, main.Path, res)
	if !res.Expanded() {
		return main, source, nil
	}
	return res.File, res.Source, nil
}

// reportInline logs what the pass did and, more importantly, what it refused
// to do. The refusals are the part a user needs: each one is a helper that
// will still arrive as a stub or a TODO downstream.
func reportInline(ctx context.Context, path string, res *inline.Result) {
	log := telemetry.Log(ctx)
	if res.Expanded() {
		names := make([]string, 0, len(res.Sites))
		for _, s := range res.Sites {
			names = append(names, fmt.Sprintf("%s(%s:%d-%d)", s.Fn, s.DefinedIn, s.FromLine, s.ToLine))
		}
		sort.Strings(names)
		log.Info("cross-file fns inlined — each converts as a controller helper (controller/fns.go), called by the controller",
			"file", path, "count", len(res.Sites), "fns", names)
	}
	for _, s := range res.Skips {
		log.Warn("fn not inlined — it keeps the legacy drop/stub handling at the call site",
			"file", path, "fn", s.Fn, "reason", string(s.Code), "detail", s.Detail)
	}
}

// inlineSummary renders the pass's outcome as one human line for the extract
// command's stdout report, next to the external_fns census it changes.
func inlineSummary(res *inline.Result) string {
	if res == nil {
		return ""
	}
	if res.Expanded() {
		return fmt.Sprintf(" inlined_fns=%d", len(res.Sites))
	}
	if len(res.Skips) > 0 {
		return fmt.Sprintf(" not_inlined=%d", len(res.Skips))
	}
	return ""
}

// expandCorpusFns applies the pass across a whole directory-mode corpus,
// replacing each entry file's IR with its expanded form. Non-entry files (fn
// libraries, fragments) are left exactly as extracted — nothing calls them,
// so there is nothing to inline into.
//
// The returned map is keyed by path, and the input slice is not mutated, so
// a caller that also wants the raw corpus (plan's FnFiles, for example) still
// has it.
func expandCorpusFns(ctx context.Context, files []*ir.File, disabled bool) (map[string]*ir.File, map[string]*inline.Result, error) {
	expanded := make(map[string]*ir.File, len(files))
	results := make(map[string]*inline.Result, len(files))
	if disabled || len(files) < 2 {
		return expanded, results, nil
	}
	sources, err := corpusSources(files)
	if err != nil {
		return nil, nil, err
	}
	corpus := inline.NewCorpus(files, sources)
	for _, f := range files {
		if f.Entry == "" || f.Entry == "__fragment" || len(f.ExternalFns) == 0 {
			continue
		}
		res, err := inline.Expand(f, corpus, inline.Options{IROptions: ir.DefaultOptions()})
		if err != nil {
			return nil, nil, err
		}
		reportInline(ctx, f.Path, res)
		results[f.Path] = res
		if res.Expanded() {
			expanded[f.Path] = res.File
		}
	}
	return expanded, results, nil
}
