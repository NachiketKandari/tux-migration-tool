package main

// templates — the user template overlay's control surface: list the
// effective set (embedded vs override origin), dump the embedded defaults
// into a directory to edit, and verify an override directory before a run
// (unknown ids, empty files, parse errors). The generators themselves take
// -templates <dir> / templates.dir; this command never generates anything.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"tux-migration-tool/internal/templates"
)

func runTemplates(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("templates: want a subcommand (list, dump, verify)")
	}
	switch args[0] {
	case "list":
		return runTemplatesList(ctx, args[1:])
	case "dump":
		return runTemplatesDump(args[1:])
	case "verify":
		return runTemplatesVerify(ctx, args[1:])
	default:
		return fmt.Errorf("templates: unknown subcommand %q (want list, dump or verify)", args[0])
	}
}

// runTemplatesList prints the effective set: every known id with its origin
// (override file vs embedded) and size, so an operator can see exactly what
// a run will render from.
func runTemplatesList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("templates list", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to .tuxconv.yaml (default: ./.tuxconv.yaml when present, else defaults)")
	dirFlag := fs.String("dir", "", "Override directory (default: templates.dir from config)")
	flagArgs, _ := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	cfg, cfgSource, err := loadRunConfig(*configPath)
	if err != nil {
		return err
	}
	logConfigRouting(ctx, cfg, cfgSource)

	dir := *dirFlag
	if dir == "" {
		dir = cfg.Templates.Dir
	}
	prov, err := templates.Resolve(dir)
	if err != nil {
		return err
	}

	overridden := 0
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSIZE\tORIGIN")
	for _, info := range templates.List(prov) {
		if info.Overridden {
			overridden++
		}
		fmt.Fprintf(w, "%s\t%d\t%s\n", info.ID, info.Bytes, info.Origin)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if dir == "" {
		fmt.Printf("\nembedded %s: %d templates, no override dir\n", templates.Version, len(templates.AllIDs))
	} else {
		fmt.Printf("\noverride dir %s: %d of %d templates overridden (rest: embedded %s)\n",
			dir, overridden, len(templates.AllIDs), templates.Version)
	}
	return nil
}

// runTemplatesDump exports the embedded set to a directory as the starting
// point for edits. Existing files are kept unless -force, so user edits are
// never clobbered by a re-dump.
func runTemplatesDump(args []string) error {
	fs := flag.NewFlagSet("templates dump", flag.ContinueOnError)
	outDir := fs.String("out", "templates", "Directory to write the embedded templates into")
	force := fs.Bool("force", false, "Overwrite existing files (default: keep them)")
	flagArgs, _ := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	written, skipped, err := templates.Dump(*outDir, *force)
	if err != nil {
		return err
	}
	fmt.Printf("templates dump: wrote %d template(s) to %s (%s)\n", len(written), *outDir, templates.Version)
	for _, path := range skipped {
		fmt.Println("  kept (already exists):", path)
	}
	if len(skipped) > 0 {
		fmt.Println("  note: re-run with -force to overwrite all of them")
	}
	fmt.Println("  next: edit the files, then `tuxconv templates verify -dir " + *outDir + "`,")
	fmt.Println("  and run a generator with -templates " + *outDir + " (or set templates.dir in .tuxconv.yaml)")
	return nil
}

// runTemplatesVerify checks an override directory without generating:
// unknown ids, empty files, and parse errors are reported per file; any
// issue exits non-zero. A clean run prints the override count.
func runTemplatesVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("templates verify", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to .tuxconv.yaml (default: ./.tuxconv.yaml when present, else defaults)")
	dirFlag := fs.String("dir", "", "Override directory (default: templates.dir from config)")
	flagArgs, _ := reorderArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	cfg, cfgSource, err := loadRunConfig(*configPath)
	if err != nil {
		return err
	}
	logConfigRouting(ctx, cfg, cfgSource)

	dir := *dirFlag
	if dir == "" {
		dir = cfg.Templates.Dir
	}
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("templates verify: no override dir — pass -dir or set templates.dir in the config")
	}
	issues, err := templates.Verify(dir)
	if err != nil {
		return err
	}
	// Count what a run would actually override, for a useful clean report.
	// (Counted from the directory itself: constructing the overlay provider
	// would abort on the very issues Verify collects.)
	known := make(map[string]bool, len(templates.AllIDs))
	for _, id := range templates.AllIDs {
		known[string(id)] = true
	}
	overridden := 0
	if entries, rerr := os.ReadDir(dir); rerr == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".tmpl") &&
				known[strings.TrimSuffix(e.Name(), ".tmpl")] {
				overridden++
			}
		}
	}
	if len(issues) == 0 {
		fmt.Printf("templates verify: %s OK — %d of %d templates overridden, rest embedded %s\n",
			dir, overridden, len(templates.AllIDs), templates.Version)
		return nil
	}
	for _, issue := range issues {
		fmt.Fprintf(os.Stderr, "  %s: %s\n", issue.Path, issue.Detail)
	}
	return fmt.Errorf("templates verify: %d issue(s) in %s", len(issues), dir)
}
