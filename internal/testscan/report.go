package testscan

import (
	"fmt"
	"io"
	"strings"
)

// Layer is one service layer gentest targets.
type Layer string

// The layer taxonomy; "other" is the degrade for a .go file outside any
// conventional layer directory, "models" is inventory-only (never a test
// target).
const (
	LayerDB         Layer = "db"
	LayerController Layer = "controller"
	LayerHandler    Layer = "handler"
	LayerModels     Layer = "models"
	LayerOther      Layer = "other"
)

// Mode is the resolved target shape.
type Mode string

const (
	ModeFile    Mode = "file"
	ModeLayer   Mode = "layer"
	ModeService Mode = "service"
	ModeRoot    Mode = "root"
)

// LayerDir is one concrete layer directory to scan, pinned to its service.
type LayerDir struct {
	Service    string
	ServiceDir string
	Layer      Layer
	Dir        string
}

// Target is a resolved gentest target: the layer directories to scan,
// grouped under their services by Scan.
type Target struct {
	Path      string
	Mode      Mode
	Root      string
	LayerDirs []LayerDir
}

// Evidence records why a function counts as tested: the kind ("name" for
// an exact Test<Fn> match, "call" for a call site in a Test-prefixed
// body), the test that provides it, and its location.
type Evidence struct {
	Kind string `json:"kind"`
	Test string `json:"test"`
	File string `json:"file"`
	Line int    `json:"line"`
}

// Func is one inventoried function of a layer and its coverage outcome.
type Func struct {
	Name     string     `json:"name"`
	Recv     string     `json:"recv,omitempty"`
	Exported bool       `json:"exported"`
	Ctor     bool       `json:"ctor,omitempty"`
	File     string     `json:"file"`
	Line     int        `json:"line"`
	HasTest  bool       `json:"has_test"`
	Evidence []Evidence `json:"evidence,omitempty"`
}

// LayerReport is one layer directory's inventory.
type LayerReport struct {
	Layer Layer  `json:"layer"`
	Dir   string `json:"dir"`
	Funcs []Func `json:"funcs"`
}

// ServiceReport groups a service's layer inventories.
type ServiceReport struct {
	Name   string        `json:"name"`
	Dir    string        `json:"dir"`
	Layers []LayerReport `json:"layers"`
}

// Report is the scan outcome: per-service layer inventories plus the
// run's skipped-file/no-file warnings.
type Report struct {
	Target   string          `json:"target"`
	Mode     Mode            `json:"mode"`
	Services []ServiceReport `json:"services"`
	Warnings []string        `json:"warnings,omitempty"`
}

// Counts returns the run-wide function, tested, and gap totals.
func (r *Report) Counts() (total, tested, gaps int) {
	for _, s := range r.Services {
		for _, l := range s.Layers {
			for _, f := range l.Funcs {
				total++
				if f.HasTest {
					tested++
				} else {
					gaps++
				}
			}
		}
	}
	return total, tested, gaps
}

// WriteText renders the human gap report — one line per function, ok with
// its evidence or gap — ending with the run totals.
func (r *Report) WriteText(w io.Writer) error {
	fmt.Fprintf(w, "gentest gap report — %s (mode %s)\n", r.Target, r.Mode)
	for _, s := range r.Services {
		fmt.Fprintf(w, "service %s (%s)\n", s.Name, s.Dir)
		for _, l := range s.Layers {
			width := 0
			for _, f := range l.Funcs {
				if len(f.Name) > width {
					width = len(f.Name)
				}
			}
			fmt.Fprintf(w, "  %s — %d functions, %d tested, %d gaps\n", l.Layer, len(l.Funcs), countTested(l.Funcs), len(l.Funcs)-countTested(l.Funcs))
			for _, f := range l.Funcs {
				if f.HasTest {
					fmt.Fprintf(w, "    ok  %-*s %s\n", width, f.Name, evidenceLine(f.Evidence))
				} else {
					fmt.Fprintf(w, "    gap %-*s (no matching test name or call site)\n", width, f.Name)
				}
			}
		}
	}
	for _, wn := range r.Warnings {
		fmt.Fprintf(w, "warning: %s\n", wn)
	}
	total, tested, gaps := r.Counts()
	fmt.Fprintf(w, "gentest: %d functions, %d tested, %d gaps\n", total, tested, gaps)
	return nil
}

func countTested(funcs []Func) int {
	n := 0
	for _, f := range funcs {
		if f.HasTest {
			n++
		}
	}
	return n
}

func evidenceLine(evs []Evidence) string {
	parts := make([]string, 0, len(evs))
	for _, e := range evs {
		parts = append(parts, fmt.Sprintf("%s (%s) %s:%d", e.Test, e.Kind, e.File, e.Line))
	}
	return strings.Join(parts, "; ")
}
