package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"tux-migration-tool/internal/analyzer"
	"tux-migration-tool/internal/audit"
	"tux-migration-tool/internal/telemetry"
)

// analysisTargets resolves the analyze targets from the positional
// arguments (user directives, 2026-09-10): an existing path passes through
// untouched (file → one report, directory → its tree); `analyze
// folder/file.pc` where the file lies deeper in the folder's tree walks it
// for a case-insensitive basename match; `analyze folder file.pc` is the
// explicit folder+name form — and the names after the folder may be
// multiple positionals or one quoted argument, comma- or space-separated
// (`analyze folder "a.pc, b.pc"`, `analyze folder a b c`). A selector may
// omit the extension (`SVC_DEMO_LIST` matches SVC_DEMO_LIST.pc). A `.txt`
// second positional (or a lone `.txt`) is a file list whose lines carry
// one-or-more comma/whitespace-separated names: every name resolves inside
// the folder tree with the same one-match rule, duplicates collapse, and
// any miss errors naming the list. Exactly one match wins everywhere; zero
// or several are loud errors — never a silent pick.
func analysisTargets(positional []string) ([]string, error) {
	if len(positional) == 1 {
		p := positional[0]
		if _, err := os.Stat(p); err == nil {
			if isFileList(p) {
				// A lone list evaluates against its own folder.
				return resolveFileList(filepath.Dir(p), p)
			}
			return []string{p}, nil
		}
		if hasSelectorSeparators(p) {
			return nil, fmt.Errorf("analyze: multiple file names need a target folder first: analyze <folder> %q", p)
		}
		dir, name := filepath.Split(p)
		dir = strings.TrimSuffix(dir, string(filepath.Separator))
		if dir == "" {
			return []string{p}, nil // no folder to search — os.Stat reports it
		}
		found, err := findAnalysisFile(dir, name)
		if err != nil {
			return nil, err
		}
		return []string{found}, nil
	}
	folder := positional[0]
	if len(positional) == 2 && isFileList(positional[1]) {
		listPath := positional[1]
		if _, err := os.Stat(listPath); err != nil {
			listPath = filepath.Join(folder, listPath)
		}
		return resolveFileList(folder, listPath)
	}
	return resolveNameSet(folder, positional[1:])
}

// resolveNameSet resolves the name arguments after the folder — each
// argument may carry one or many comma/whitespace-separated names — with
// the single-file matcher, in argument order, duplicates collapsed.
func resolveNameSet(folder string, args []string) ([]string, error) {
	var names []string
	for _, arg := range args {
		names = append(names, splitNameSelectors(arg)...)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("analyze: no file names given after %q", folder)
	}
	var out []string
	seen := map[string]bool{}
	for _, name := range names {
		found, err := findAnalysisFile(folder, name)
		if err != nil {
			return nil, err
		}
		if !seen[found] {
			seen[found] = true
			out = append(out, found)
		}
	}
	return out, nil
}

// splitNameSelectors splits one selector argument (or a file-list line)
// into individual names: commas and whitespace are both separators.
func splitNameSelectors(arg string) []string {
	return strings.FieldsFunc(arg, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}

// hasSelectorSeparators reports whether the argument carries comma or
// whitespace separators — a multi-name selector without a folder.
func hasSelectorSeparators(arg string) bool {
	return strings.ContainsAny(arg, ", \t")
}

// isFileList reports whether the argument names a newline-separated file
// list (.txt) — the analyze name-list selector form.
func isFileList(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".txt")
}

// resolveFileList reads the name list and resolves every name inside dir's
// tree with the single-file matcher (case-insensitive, extension optional,
// one-match-wins). Lines carry one-or-more comma/whitespace-separated
// names; blank lines and #-comments (inline too) are skipped; duplicate
// resolutions collapse to one report.
func resolveFileList(dir, listPath string) ([]string, error) {
	data, err := os.ReadFile(listPath)
	if err != nil {
		return nil, fmt.Errorf("analyze: reading file list %s: %w", listPath, err)
	}
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i] // inline comments ride the # convention
		}
		for _, name := range splitNameSelectors(line) {
			found, err := findAnalysisFile(dir, name)
			if err != nil {
				return nil, fmt.Errorf("analyze: file list %s: %w", listPath, err)
			}
			if !seen[found] {
				seen[found] = true
				out = append(out, found)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("analyze: file list %s resolves to no files", listPath)
	}
	return out, nil
}

// findAnalysisFile walks dir recursively for the one .pc/.pcf file whose
// base name matches the selector (extension optional, case-insensitive).
func findAnalysisFile(dir, name string) (string, error) {
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("cannot access folder %s: %w", dir, err)
	}
	want := strings.ToLower(strings.TrimSpace(name))
	wantStem := strings.ToLower(strings.TrimSuffix(want, filepath.Ext(want)))
	var matches []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".pc" && ext != ".pcf" {
			return nil
		}
		base := strings.ToLower(filepath.Base(path))
		if base == want || strings.TrimSuffix(base, ext) == wantStem {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("searching %s: %w", dir, err)
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no .pc/.pcf file matching %q under %s", name, dir)
	default:
		return "", fmt.Errorf("%q is ambiguous under %s — %d files match, name one exactly: %s",
			name, dir, len(matches), strings.Join(matches, ", "))
	}
}

func runAnalyze(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	csvPath := fs.String("csv", "", "Path to export CSV report (defaults to stdout)")
	weightsPath := fs.String("weights", "", "Path to a previously generated analysis CSV whose external_fns weights override the defaults (edit the CSV and re-run to re-score)")
	pattern := fs.String("pattern", "", "Directory mode only: analyze only the .pc/.pcf files whose base name contains this substring (case-insensitive), e.g. -pattern px_")

	flagArgs, positional := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}

	opts := analyzer.DefaultOptions()
	if *weightsPath != "" {
		loaded, err := analyzer.LoadOptionsCSV(*weightsPath)
		if err != nil {
			return fmt.Errorf("failed loading weights CSV %s: %w", *weightsPath, err)
		}
		opts = loaded
		telemetry.Log(ctx).Info("loaded scoring overrides",
			"path", *weightsPath,
			"marks", fmt.Sprintf("query=%d simple=%d complex=%d tpcall=%d branch=%d tier_high=%d tier_medium=%d",
				opts.Marks.Query, opts.Marks.Simple, opts.Marks.Complex, opts.Marks.TpCall,
				opts.Marks.Branch, opts.Marks.TierHigh, opts.Marks.TierMedium),
			"fn_overrides", len(opts.FnWeights))
	}

	remaining := positional
	if len(remaining) == 0 {
		return fmt.Errorf("must provide a file or directory to analyze")
	}

	paths, err := analysisTargets(remaining)
	if err != nil {
		return err
	}
	telemetry.Log(ctx).Info("analyze invoked", "targets", len(paths), "first", paths[0], "csv", *csvPath)

	dirMode := false
	var reports []*analyzer.Report
	if len(paths) == 1 {
		fi, err := os.Stat(paths[0])
		if err != nil {
			return fmt.Errorf("cannot access target path %s: %w", paths[0], err)
		}
		if !fi.IsDir() {
			if strings.TrimSpace(*pattern) != "" {
				return fmt.Errorf("analyze: -pattern applies to directory targets only (passed %q)", paths[0])
			}
			rep, err := analyzer.AnalyzeFile(paths[0], opts)
			if err != nil {
				return err
			}
			reports = []*analyzer.Report{rep}
		} else {
			dirMode = true
		}
	}

	if dirMode {
		targetPath := paths[0]
		reps, err := analyzer.AnalyzeDir(targetPath, opts)
		if err != nil {
			return err
		}
		if needle := strings.TrimSpace(*pattern); needle != "" {
			kept := make([]*analyzer.Report, 0, len(reps))
			for _, r := range reps {
				if strings.Contains(strings.ToLower(filepath.Base(r.File)), strings.ToLower(needle)) {
					kept = append(kept, r)
				}
			}
			if len(kept) == 0 {
				return fmt.Errorf("analyze: no .pc/.pcf file in %s matches -pattern %q", targetPath, needle)
			}
			telemetry.Log(ctx).Info("analyze pattern applied",
				"pattern", needle, "matched", len(kept), "of", len(reps))
			reps = kept
		}
		reports = reps
	} else if len(paths) > 1 {
		if strings.TrimSpace(*pattern) != "" {
			return fmt.Errorf("analyze: -pattern applies to directory targets only (a file list selects its own files)")
		}
		for _, p := range paths {
			rep, err := analyzer.AnalyzeFile(p, opts)
			if err != nil {
				return err
			}
			reports = append(reports, rep)
		}
	}

	if len(reports) == 0 {
		fmt.Fprintf(os.Stderr, "no .pc or .pcf files found in %s\n", strings.Join(paths, ", "))
		return nil
	}

	archiveTriageCSV(ctx, reports, opts.Marks)
	telemetry.Log(ctx).Info("analysis complete", "files", len(reports))

	var out io.Writer = os.Stdout
	if *csvPath != "" {
		f, err := os.Create(*csvPath)
		if err != nil {
			return fmt.Errorf("failed creating CSV output file %s: %w", *csvPath, err)
		}
		defer f.Close()
		out = f
		telemetry.Log(ctx).Info("writing analysis report to CSV", "path", *csvPath, "records", len(reports))
	}

	if err := analyzer.WriteCSV(out, reports, opts.Marks); err != nil {
		return fmt.Errorf("failed writing CSV report: %w", err)
	}

	if *csvPath != "" {
		fmt.Printf("Wrote analysis report for %d files to %s\n", len(reports), *csvPath)
	}
	if *weightsPath == "" {
		target := *csvPath
		if target == "" {
			target = "<file>.csv (save with -csv)"
		}
		fmt.Fprintf(os.Stderr, "Tip: to re-score, edit the '# tuxconv marks' line or the external_fns weights in %s, then re-run with -weights %s\n", target, target)
	}
	return nil
}

// archiveTriageCSV persists the run's report to conversion_logs/audit/<run-id>/
// via the audit Recorder (§4.7). Best-effort: archival failures are logged,
// never fatal.
func archiveTriageCSV(ctx context.Context, reports []*analyzer.Report, marks analyzer.Marks) {
	log := telemetry.Log(ctx)
	rec, err := audit.New(auditDir, telemetry.RunIDFromContext(ctx))
	if err != nil {
		log.Warn("audit archive unavailable", "error", err)
		return
	}
	path, err := rec.Write("triage_report.csv", func(w io.Writer) error {
		return analyzer.WriteCSV(w, reports, marks)
	})
	if err != nil {
		log.Warn("audit archive write failed", "error", err)
		return
	}
	log.Info("analysis archived", "path", path)
}
