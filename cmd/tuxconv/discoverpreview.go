package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"tux-migration-tool/internal/config"
	"tux-migration-tool/internal/flow"
	"tux-migration-tool/internal/ir"
)

// FilterPreviewJSON is the machine-readable scenarioFilter preview for the
// web playground (discover -filter <expr> -filter-json <path>): the same
// ground truth as the console text plus the flattened source the console
// never prints. The frontend renders this verbatim — no expression parsing
// in the browser.
type FilterPreviewJSON struct {
	Entry      string   `json:"entry"`
	Filter     string   `json:"filter"`
	MergedKey  string   `json:"mergedKey"`
	Matched    []string `json:"matched"`
	Pruned     []string `json:"pruned,omitempty"`
	Blocks     [][2]int `json:"blocks"`
	BlockLines int      `json:"blockLines"`
	Kept       int      `json:"kept"`
	Dropped    int      `json:"dropped"`
	Unfolded   int      `json:"unfolded"`
	Reads      []string `json:"reads"`
	Writes     []string `json:"writes"`
	Queries    []string `json:"queries"`
	Tx         []string `json:"tx,omitempty"`
	Residue    []string `json:"residue,omitempty"`
	LogicOnly  bool     `json:"logicOnly"`
	Flattened  string   `json:"flattened"`
	Error      string   `json:"error,omitempty"`
}

// FilterPreviewFile is the -filter-json document: one preview per entry
// (the web job has one entry; directories get one element per file).
type FilterPreviewFile struct {
	Filter   string              `json:"filter"`
	Previews []FilterPreviewJSON `json:"previews"`
}

// discoverPreview implements the read-only discover modes (scenario-filter
// plan §5): -list-axes prints each entry's ranked dispatch-axis registry,
// -filter previews a scenarioFilter expression's resolution — the merged
// key, matched/pruned assignments, kept/dropped fold counts, queries, and
// the honest kept blocks — without writing a draft or an artifact. Both
// modes are the fast authoring loop before a mapping is touched; convert
// and validate re-check the same expression at plan time.
//
// When filterJSON is set, the same fold also lands as JSON (one preview per
// entry, including the flattened .pc via flow.RenderScenario) so the web
// playground can show real code instead of re-parsing console text.
func discoverPreview(target string, cfg *config.Config, listAxes bool, filterExpr, filterJSON string) error {
	fi, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("cannot access target path %s: %w", target, err)
	}
	dirMode := fi.IsDir()

	irFiles, err := extractFlowIR(target, cfg)
	if err != nil {
		return err
	}
	if len(irFiles) == 0 {
		return fmt.Errorf("no .pc or .pcf files found in %s", target)
	}
	var filter *flow.ScenarioFilter
	if filterExpr != "" {
		filter, err = flow.ParseScenarioFilter(filterExpr)
		if err != nil {
			return err
		}
	}
	fmt.Printf("read-only preview — no drafts or artifacts written\n\n")
	var firstErr error
	var jsonPreviews []FilterPreviewJSON
	for _, f := range irFiles {
		if f.Entry == "" {
			fmt.Printf("- %s: no entry function (fn library) — skipped\n", filepath.Base(f.Path))
			if filterJSON != "" && filter != nil {
				jsonPreviews = append(jsonPreviews, FilterPreviewJSON{
					Entry:  filepath.Base(f.Path),
					Filter: filter.Text,
					Error:  "no entry function (fn library) — skipped",
				})
			}
			continue
		}
		src, err := os.ReadFile(f.Path)
		if err != nil {
			return fmt.Errorf("discover: read %s: %w", f.Path, err)
		}
		facts, err := flow.ScanForIR(string(src), f)
		if err != nil {
			return fmt.Errorf("discover: scan %s: %w", f.Path, err)
		}
		tree := flow.Build(src, facts, f.Entry, f)
		axes := tree.AxesFor(src)
		if listAxes {
			fmt.Print(renderAxesList(filepath.Base(f.Path), axes))
		}
		if filter == nil {
			continue
		}
		out, perr := renderFilterPreview(filepath.Base(f.Path), tree, axes, filter)
		if perr != nil {
			if filterJSON != "" {
				jsonPreviews = append(jsonPreviews, FilterPreviewJSON{
					Entry:  filepath.Base(f.Path),
					Filter: filter.Text,
					Error:  perr.Error(),
				})
			}
			if !dirMode {
				// Still write the JSON envelope so the web playground gets a
				// structured error instead of an empty file.
				if filterJSON != "" {
					_ = writeFilterPreviewJSON(filterJSON, filter.Text, jsonPreviews)
				}
				return perr
			}
			fmt.Printf("- %s: %v\n", filepath.Base(f.Path), perr)
			if firstErr == nil {
				firstErr = perr
			}
			continue
		}
		fmt.Print(out)
		if filterJSON != "" {
			jsonPreviews = append(jsonPreviews, buildFilterPreviewJSON(filepath.Base(f.Path), tree, axes, filter, src, f))
		}
	}
	if listAxes || filter != nil {
		fmt.Println()
	}
	if filterJSON != "" && filter != nil {
		if err := writeFilterPreviewJSON(filterJSON, filterExpr, jsonPreviews); err != nil {
			return err
		}
		fmt.Printf("filter preview JSON: %s (%d entr%s)\n", filterJSON, len(jsonPreviews), pluralS(len(jsonPreviews), "y", "ies"))
	}
	return firstErr
}

// pluralS renders a count-noun suffix ("1 entry", "2 entries").
func pluralS(n int, sing, plur string) string {
	if n == 1 {
		return sing
	}
	return plur
}

// buildFilterPreviewJSON folds the same ScenarioForFilter ground truth the
// console prints and adds the flattened .pc (flow.RenderScenario) the
// console never prints — the web playground's code view.
func buildFilterPreviewJSON(entry string, tree *flow.Tree, axes []*flow.DispatchAxis, filter *flow.ScenarioFilter, src []byte, irFile *ir.File) FilterPreviewJSON {
	p := FilterPreviewJSON{Entry: entry, Filter: filter.Text}
	sc, err := flow.ScenarioForFilter(tree, axes, filter)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	blocks := flow.KeptBlocks(sc, tree)
	p.MergedKey = sc.Key
	p.Matched = append([]string(nil), sc.FilterMatched...)
	p.Pruned = append([]string(nil), sc.FilterPruned...)
	p.Blocks = append([][2]int(nil), blocks...)
	p.BlockLines = flow.KeptBlockLines(blocks)
	p.Kept = sc.Counts.Kept
	p.Dropped = sc.Counts.Dropped
	p.Unfolded = sc.Counts.Unfolded
	p.Reads = append([]string(nil), sc.Gets...)
	p.Writes = append([]string(nil), sc.Adds...)
	p.Queries = scenarioQueryIDs(sc)
	p.Tx = scenarioTxIDs(sc)
	p.Residue = append([]string(nil), sc.Residue...)
	p.LogicOnly = !sc.CarriesContract()
	// entryBase mirrors scenarioArtifacts' entry (f.Entry, e.g. SVC_X) so
	// the flattened header matches the slice artifacts.
	entryName := entry
	if irFile != nil && irFile.Entry != "" {
		entryName = irFile.Entry
	}
	p.Flattened = flow.RenderScenario(sc, tree, entryName, src, irFile)
	return p
}

// writeFilterPreviewJSON persists the -filter-json envelope (mkdir -p the
// parent so the web job's tmp path never fails on a missing dir).
func writeFilterPreviewJSON(path, filter string, previews []FilterPreviewJSON) error {
	if previews == nil {
		previews = []FilterPreviewJSON{}
	}
	data, err := json.MarshalIndent(FilterPreviewFile{Filter: filter, Previews: previews}, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("filter-json: mkdir %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("filter-json: write %s: %w", path, err)
	}
	return nil
}

// renderAxesList renders one entry's registry for -list-axes: rank, key,
// kind, domain (default arm marked), predicate-site count, and guard lines.
func renderAxesList(entry string, axes []*flow.DispatchAxis) string {
	var sb strings.Builder
	if len(axes) == 0 {
		fmt.Fprintf(&sb, "- %s: no dispatch axis detected\n", entry)
		return sb.String()
	}
	fmt.Fprintf(&sb, "- %s: %d dispatch axis(es) — scenarioFilter variables\n", entry, len(axes))
	for i, a := range axes {
		domain := strings.Join(a.Domain, ",")
		if a.HasDefault {
			domain += " +default(" + a.DefaultKey() + ")"
		}
		alias := a.Alias
		if alias == "" {
			alias = "—"
		}
		fmt.Fprintf(&sb, "    rank %d  %-22s %-9s ref %-22s alias %-14s domain %-18s sites %d  guards %s\n",
			i, a.Key(), a.Kind, a.Ref, alias, domain, a.Sites, joinLineNums(a.GuardLines))
	}
	return sb.String()
}

// renderFilterPreview renders one entry's scenarioFilter resolution: the
// merged key, the matched assignments, the fold counts, the honest kept
// blocks (never BodyExtent's min-max span), the FML census, queries, tx
// calls, and loud residue.
func renderFilterPreview(entry string, tree *flow.Tree, axes []*flow.DispatchAxis, filter *flow.ScenarioFilter) (string, error) {
	if len(axes) == 0 {
		return "", fmt.Errorf("scenarioFilter %q: the entry has no dispatch axes", filter.Text)
	}
	sc, err := flow.ScenarioForFilter(tree, axes, filter)
	if err != nil {
		return "", err
	}
	blocks := flow.KeptBlocks(sc, tree)
	var sb strings.Builder
	fmt.Fprintf(&sb, "- %s\n", entry)
	fmt.Fprintf(&sb, "  scenarioFilter: %s\n", filter.Text)
	fmt.Fprintf(&sb, "  merged key: %s\n", sc.Key)
	fmt.Fprintf(&sb, "  matched assignments: %s\n", strings.Join(sc.FilterMatched, ", "))
	if len(sc.FilterPruned) > 0 {
		fmt.Fprintf(&sb, "  pruned at plan time: %s\n", strings.Join(sc.FilterPruned, ", "))
	}
	fmt.Fprintf(&sb, "  kept blocks: %s (%d lines)\n", formatBlocks(blocks), flow.KeptBlockLines(blocks))
	fmt.Fprintf(&sb, "  fold: kept %d nodes, dropped %d, unfolded %d\n",
		sc.Counts.Kept, sc.Counts.Dropped, sc.Counts.Unfolded)
	fmt.Fprintf(&sb, "  reads: %s\n  writes: %s\n", fieldsOrDash(sc.Gets), fieldsOrDash(sc.Adds))
	if qids := scenarioQueryIDs(sc); len(qids) > 0 {
		fmt.Fprintf(&sb, "  queries: %d (%s)\n", len(qids), strings.Join(qids, ","))
	} else {
		sb.WriteString("  queries: none\n")
	}
	if tx := scenarioTxIDs(sc); len(tx) > 0 {
		fmt.Fprintf(&sb, "  tx: %s\n", strings.Join(tx, ","))
	}
	if len(sc.Residue) > 0 {
		fmt.Fprintf(&sb, "  residue (kept verbatim — verify manually): %s\n", strings.Join(sc.Residue, "; "))
	}
	if !sc.CarriesContract() {
		sb.WriteString("  note: no FML traffic — the slice is logic-only\n")
	}
	sb.WriteString("\n")
	return sb.String(), nil
}

// writeFilterExamples renders the draft's commented scenarioFilter samples
// (plan §5): registry-derived, fold-verified, and always commented — an
// example never becomes an active endpoint on its own.
func writeFilterExamples(sb *strings.Builder, tree *flow.Tree, axes []*flow.DispatchAxis) {
	exs := flow.FilterExamples(tree, axes)
	if len(exs) == 0 {
		return
	}
	sb.WriteString("\n# scenarioFilter examples — registry-derived, verified against the source.\n")
	sb.WriteString("# A filter re-folds the tree under its matching assignments; surviving arms\n")
	sb.WriteString("# keep their runtime guards. Uncomment one and set name/route:\n")
	for _, ex := range exs {
		fmt.Fprintf(sb, "# - scenarioFilter: %s\n", strconv.Quote(ex.Expr))
		fmt.Fprintf(sb, "#     merges to %s | matched: %s\n", ex.Key, strings.Join(ex.Matched, ", "))
		fmt.Fprintf(sb, "#     kept %d lines in %d block(s): %s | reads %d | writes %d | queries %d\n",
			ex.Lines, len(ex.Blocks), formatBlocks(ex.Blocks), ex.Reads, ex.Writes, ex.Queries)
	}
}

// formatBlocks renders block extents compactly ("352-577, 977-981"; "none"
// for an empty list).
func formatBlocks(blocks [][2]int) string {
	if len(blocks) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		parts = append(parts, fmt.Sprintf("%d-%d", b[0], b[1]))
	}
	return strings.Join(parts, ", ")
}

// joinLineNums renders guard lines compactly ("4, 6, 8").
func joinLineNums(vals []int) string {
	if len(vals) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(vals))
	for _, v := range vals {
		parts = append(parts, strconv.Itoa(v))
	}
	return strings.Join(parts, ", ")
}
