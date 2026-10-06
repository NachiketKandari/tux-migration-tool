# tuxconv — executable command reference

Command-only companion to [README.md](README.md). Run from the repository root.

## Build

The CLI is one self-contained binary — no cgo, no C compiler, and the
tree-sitter grammar is embedded in it, so nothing else is needed at runtime.

```sh
go build -o tuxconv ./cmd/tuxconv    # repo-root executable
go install ./cmd/tuxconv             # or install to $GOBIN (then drop the ./ below)
```

On Windows use `-o tuxconv.exe` and run `.\tuxconv.exe`. Requires Go 1.26.4+
(`go.mod` pins it). The first build downloads three modules; after that
builds are offline.

## First five minutes

Both targets below do the same two-step dance: the **first** run has no
mapping, so it scans and writes editable drafts to `mappings/` and stops.
Review the drafts, then **re-run** the identical command to convert.

```sh
# Go service (a .pc with a SVC_* entry function)
./tuxconv convertgo path/to/SVC_FOO.pc          # run 1: writes mappings/SVC_FOO.mapping.yaml
./tuxconv convertgo path/to/SVC_FOO.pc          # run 2: converts

# Python batch (a .pc with main(), no SVC_* entry)
./tuxconv convertbatchpy path/to/BAT_FOO.pc -no-llm

# C# (.NET Core)
./tuxconv convertcs path/to/SVC_FOO.pc          # run 1: writes the .cs mapping draft
./tuxconv convertcs path/to/SVC_FOO.pc          # run 2: converts
```

`-no-llm` forces the deterministic path everywhere — no keys, no network,
fully reproducible. Drop it to let the configured model fill the bodies
that deterministic mode leaves as drafts or `TODO` seams.

Output lands in `conversion_logs/` (gitignored): `_staged/<service>/` for
generated code, plus `logs/`, `audit/`, and `ledger/`.

## General

```sh
./tuxconv                            # usage
./tuxconv -h
./tuxconv help
./tuxconv version

./tuxconv -verbose <command> ...        # debug-level console logging (file log always captures debug)
./tuxconv -log-dir <dir> <command> ...  # structured JSONL logs (default: conversion_logs/logs)
```

## Parse, plan, inspect

```sh
./tuxconv extract <file|dir> [-out <path>] [-fragment] [-no-inline-fns] [-config <path>]
./tuxconv plan <file> -mapping <yaml> [-ledger <dir>] [-fragment] [-no-inline-fns] [-config <path>]
./tuxconv flow <file|dir> [-go] [-scenarios] [-scenarios-dir <dir>] [-out <report.json>] [-config <path>]
./tuxconv analyze <file|dir> [-csv <out.csv>] [-weights <csv>] [-pattern <substr>]
./tuxconv walkreport <file|dir> [-controller <dir>] [-json] [-out <path>] [-strict] [-config <path>]
./tuxconv dbcheck [-ping] [-config <path>]
```

Analyze selectors:

```sh
./tuxconv analyze <dir>/<file.pc>
./tuxconv analyze <dir> <file.pc>
./tuxconv analyze <dir> "a.pc, b.pc"
./tuxconv analyze <dir> a.pc b.pc
./tuxconv analyze files.txt
./tuxconv analyze <dir> -pattern px_
./tuxconv analyze <dir> -csv out.csv
./tuxconv analyze <dir> -weights out.csv -csv rescored.csv
```

## Mapping drafts and naming

```sh
./tuxconv discover <file|dir>                          # write draft yaml(s) to mappings/
./tuxconv discover <file|dir> -stdout                  # print the draft(s) instead
./tuxconv discover <file|dir> -out <dir>
./tuxconv discover <file|dir> -no-llm                  # deterministic names
./tuxconv discover <file|dir> -target cs               # .NET Core mapping schema
./tuxconv discover <file|dir> -list-axes               # dispatch-axis registry (read-only)
./tuxconv discover <file|dir> -filter "<expr>"         # scenarioFilter fold preview (read-only)
./tuxconv discover <file|dir> -filter "<expr>" -filter-json <path>
./tuxconv ainames <file> -mapping <yaml>               # AI-name surviving endpoints, in place
./tuxconv ainames <file> -mapping <yaml> -all          # re-name entries already ai-suggested
```

## Convert

```sh
./tuxconv convertgo <file|dir>                         # first run: draft mappings/ and stop
./tuxconv convertgo <file|dir> -mapping <yaml|dir> [-no-llm] [-base <dir>] [-fragment] [-retry-repair] [-no-inline-fns] [-config <path>] [-templates <dir>]
./tuxconv convertbatchpy <file|dir> [-no-llm] [-shape auto|repo] [-dml-loop batch|rowbyrow] [-out <dir>] [-config <path>] [-templates <dir>]
./tuxconv convertcs <file|dir> [-mapping <yaml|dir>] [-no-llm] [-out <dir>] [-config <path>] [-templates <dir>]
```

## Generated tests

```sh
./tuxconv gentest <converted tree> [-check-only] [-no-llm] [-layers db,controller,handler] [-out <dir>] [-base <dir>] [-in-place] [-log-file <path>] [-nice-names] [-config <path>] [-templates <dir>]
./tuxconv pygentest <file|dir> [-module-dir <dir>] [-out <dir>] [-no-run] [-strict] [-shape auto|repo] [-dml-loop batch|rowbyrow] [-python <bin>] [-config <path>] [-templates <dir>]
```

## Templates

```sh
./tuxconv templates list [-dir <override dir>] [-config <path>]
./tuxconv templates dump [-out <dir>] [-force]
./tuxconv templates verify -dir <override dir> [-config <path>]
```

## Audit

```sh
./tuxconv retrystats <audit-run-dir> [<audit-run-dir-B>]
```

## Config

```sh
./tuxconv <command> -config <path>
TUXCONV_CONFIG=<path> ./tuxconv <command>
```

## Worked examples (deterministic)

```sh
# A service source: run 1 drafts the mapping, run 2 converts it
./tuxconv convertgo path/to/SVC_FOO.pc -no-llm
./tuxconv convertgo path/to/SVC_FOO.pc -no-llm

# A source directory with its mappings: one pass per service, 0 SQL deviations
./tuxconv convertgo path/to/src -mapping path/to/mappings -no-llm

# .NET Core component tree
./tuxconv convertcs path/to/SVC_FOO.pc -mapping mappings/SVC_FOO.cs.mapping.yaml -no-llm

# .NET Core mapping draft
./tuxconv discover path/to/src -target cs

# batch → Python, then the assertion gate
./tuxconv convertbatchpy <dir> -no-llm
./tuxconv pygentest <dir> -module-dir python_out

# template overrides
./tuxconv templates dump -out templates
./tuxconv templates verify -dir templates
./tuxconv convertgo <src.pc> -templates templates -no-llm
```

## Reset for a clean re-run

```sh
rm -f mappings/<entry>.mapping.yaml
rm -rf conversion_logs/_staged/<stem> conversion_logs/ledger/<stem>.ledger.json
./tuxconv convertgo <dir>/<entry>.pc -no-llm   # drafts mappings/, defers conversion
./tuxconv convertgo <dir>/<entry>.pc -no-llm   # consumes the mapping, stages code + logs
```

## Debug CLI (build tag `tools`)

```sh
go build -tags tools -o xtux ./cmd/xtux
./xtux scan <file>
./xtux ir <file|dir>
./xtux ir <file> -fragment
```
