package main

// commandFlags is the single home for reorderArgs' value-flag knowledge.
//
// Before: main.go:deriveValueFlags re-registered every subcommand's FlagSet
// into throwaway sets and sniffed DefValue != "false"/"true" to detect value
// flags (~70L mirror + fragile string sniffing). Any flag rename needed 2
// edits or reorderArgs silently mis-split; `ainames` was missing entirely so
// `-mapping <file>` space-form broke for that subcommand only.
//
// After: one declarative table. deriveValueFlags() in main.go builds from
// this. When adding a flag, add it here AND to the subcommand's FlagSet.
// Keep names in sync — the table is the contract reorderArgs relies on.
var commandFlags = map[string][]string{
	// value flags only (bool flags omitted — they never consume next arg)
	"extract":        {"out", "config"},
	"plan":           {"mapping", "config", "ledger"},
	"convertgo":      {"mapping", "config", "base", "templates"},
	"discover":       {"out", "config", "target", "filter", "filter-json"},
	"convertbatchpy": {"out", "config", "shape", "dml-loop", "templates"},
	"pygentest":      {"module-dir", "out", "python", "config", "shape", "dml-loop", "templates"},
	"convertcs":      {"out", "mapping", "config", "templates"},
	"analyze":        {"csv", "weights", "pattern"},
	"gentest":        {"layers", "base", "out", "log-file", "config", "templates"},
	"ainames":        {"mapping", "config"},
	"templates":      {"out", "config", "dir"},
	"flow":           {"out", "scenarios-dir", "config"},
	"dbcheck":        {"config"},
	"walkreport":     {"controller", "out", "config"},
}
