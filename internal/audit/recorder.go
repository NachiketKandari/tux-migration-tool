// Package audit records the per-run audit trail under .tuxconv/audit/<run-id>/
// (PRD §4.7, architecture.md §4). Phase 1 extends the Recorder with per-unit
// traces (template ID, assembled prompt, raw LLM response, validation
// outcome); today it archives run-level artifacts such as the triage CSV.
package audit

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Recorder owns one run's audit folder.
type Recorder struct {
	mu  sync.Mutex
	dir string
}

// New creates the run's audit folder root/runID and returns a Recorder over it.
func New(root, runID string) (*Recorder, error) {
	if root == "" || runID == "" {
		return nil, fmt.Errorf("audit: root and runID must be non-empty")
	}
	dir := filepath.Join(root, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("audit: create %s: %w", dir, err)
	}
	return &Recorder{dir: dir}, nil
}

// Dir returns the run's audit folder.
func (r *Recorder) Dir() string { return r.dir }

// Write persists one named artifact by streaming it through produce and
// returns the written path. name must be a plain file name — nested per-unit
// folders get a dedicated API in Phase 1. Safe for concurrent callers (the
// dir-mode convert fan-out shares one Recorder across service workers).
func (r *Recorder) Write(name string, produce func(w io.Writer) error) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("audit: invalid artifact name %q", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	path := filepath.Join(r.dir, name)
	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("audit: create %s: %w", path, err)
	}
	defer f.Close()
	if err := produce(f); err != nil {
		return "", fmt.Errorf("audit: write %s: %w", path, err)
	}
	return path, nil
}
