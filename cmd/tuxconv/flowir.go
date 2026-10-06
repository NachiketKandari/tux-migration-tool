package main

import (
	"fmt"
	"os"

	"tux-migration-tool/internal/config"
	"tux-migration-tool/internal/ir"
)

// extractFlowIR resolves the target (file or directory) through the
// extraction path so fragments and the buffer registry apply identically —
// via irOptions, the same config seam extract/plan/convertgo use
// (engine-wiring audit Tier-1 #4: an empty registry marks every FML buffer
// unknown-role, so discover's error-add idiom never fires and the response
// census inflates).
func extractFlowIR(target string, cfg *config.Config) ([]*ir.File, error) {
	fi, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("cannot access target path %s: %w", target, err)
	}
	if fi.IsDir() {
		return ir.ExtractDirOpts(target, irOptions(cfg, false))
	}
	f, err := ir.ExtractFileOpts(target, irOptions(cfg, false))
	if err != nil {
		return nil, err
	}
	return []*ir.File{f}, nil
}
