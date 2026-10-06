//go:build tools

// Command xtux inspects the experimental scanner/IR layer: it dumps
// scanner facts or extracted IR as JSON for a file or directory.
//
// Debug-only: excluded from default builds (it links a second copy of the
// tree-sitter runtime for local inspection; `tuxconv extract` covers the
// same ground in the shipped binary). Build/run with:
//   go run -tags tools ./cmd/xtux scan|ir <file|dir> [-fragment]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"tux-migration-tool/internal/ir"
	"tux-migration-tool/internal/tsscan"
)

func main() {
	frag := flag.Bool("fragment", false, "force the fragment rubric (file mode)")
	flag.Parse()
	if flag.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "usage: xtux scan|ir <file|dir> [-fragment]")
		os.Exit(2)
	}
	cmd, target := flag.Arg(0), flag.Arg(1)
	switch cmd {
	case "scan":
		facts, err := tsscan.ScanFile(target)
		if err != nil {
			fail(err)
		}
		dump(facts)
	case "ir":
		info, err := os.Stat(target)
		if err != nil {
			fail(err)
		}
		if info.IsDir() {
			files, err := ir.ExtractDir(target)
			if err != nil {
				fail(err)
			}
			for _, f := range files {
				dump(f)
			}
			return
		}
		opts := ir.DefaultOptions()
		opts.ForceFragment = *frag
		f, err := ir.ExtractFileOpts(target, opts)
		if err != nil {
			fail(err)
		}
		dump(f)
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", cmd)
		os.Exit(2)
	}
}

func dump(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "xtux:", err)
	os.Exit(1)
}
