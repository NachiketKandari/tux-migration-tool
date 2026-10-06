package main

// discovercs — the `discover -target cs` draft path (B1/B2): the same
// scan-then-tag contract as the Go drafts, rendered for the convertcs
// mapping schema. Deterministic naming throughout (no AI pass): one
// endpoint per qualifying scenario slice (or API candidate on non-axis
// files), namespace/area/component placeholders filled from the
// convertcs config defaults when set, requestFields/paramNames from the
// FML read targets, and dbMethods const-name pins — zero hand-written
// yaml for a new file's first pass.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"tux-migration-tool/internal/config"
	"tux-migration-tool/internal/csdraft"
	"tux-migration-tool/internal/flow"
	"tux-migration-tool/internal/telemetry"
)

// discoverCsCore renders one cs mapping draft per entry file into out (or
// prints with stdout). The draft file name is <stem>.cs.mapping.yaml;
// never clobbers — a kept draft gets a numbered sibling.
func discoverCsCore(ctx context.Context, target, out string, stdout bool, cfg *config.Config, opts csdraft.Options) (int, error) {
	log := telemetry.Log(ctx)

	if _, err := os.Stat(target); err != nil {
		return 0, fmt.Errorf("cannot access target path %s: %w", target, err)
	}
	irFiles, err := extractFlowIR(target, cfg)
	if err != nil {
		return 0, err
	}
	if len(irFiles) == 0 {
		return 0, fmt.Errorf("no .pc or .pcf files found in %s", target)
	}

	written, existing := 0, 0
	for _, f := range irFiles {
		if f.Entry == "" {
			fmt.Printf("- %s: no entry function (fn library) — skipped\n", filepath.Base(f.Path))
			continue
		}
		src, err := os.ReadFile(f.Path)
		if err != nil {
			return written, fmt.Errorf("discover: read %s: %w", f.Path, err)
		}
		facts, err := flow.ScanForIR(string(src), f)
		if err != nil {
			return written, fmt.Errorf("discover: scan %s: %w", f.Path, err)
		}
		tree := flow.Build(src, facts, f.Entry, f)
		if axis := tree.DispatchAxisFor(src); axis != nil {
			printScenarioSummary(f, axis, flow.Scenarios(tree, axis))
		} else {
			fmt.Printf("\n%s: no dispatch axis — drafting the condition-inventory candidates\n", f.Entry)
		}
		draft := csdraft.Render(f, tree, src, opts)
		if stdout {
			fmt.Println()
			fmt.Println(draft)
			written++
			continue
		}
		stem := strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path))
		path, kept, err := writeDraft(out, stem+".cs.mapping.yaml", draft)
		if err != nil {
			return written, err
		}
		if kept {
			existing++
		}
		written++
		log.Info("cs draft written", "path", path)
		fmt.Printf("  draft: %s\n", path)
	}
	if stdout {
		return written, nil
	}
	fmt.Printf("\n%d cs draft(s) written to %s (%d existing kept) — review namespace/component and name/route, then: tuxconv convertcs %s -mapping <draft>\n",
		written, out, existing, target)
	return written, nil
}
