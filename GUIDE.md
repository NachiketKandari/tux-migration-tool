# GUIDE — a map of this repository

[README.md](README.md) explains what the tool does and the guarantees it makes;
[README-executable.md](README-executable.md) is the full command reference. This
file sits between them: how to get a conversion running, how the code is
layered, and where to look when you want to change something.

## What it is

Pro*C / Tuxedo C in; Go services, Python batch programs, or a C# (.NET Core)
component tree out. The C grammar is vendored as WASM and runs on the pure-Go
wazero runtime, so `tuxconv` is one binary with no cgo, no C toolchain and no
platform-specific build step. Conversion is deterministic; an LLM profile is
optional and only fills the bodies deterministic mode leaves as drafts.

## Sixty seconds to output

Go 1.26.4 or newer, then:

```bash
go build -o tuxconv ./cmd/tuxconv
./tuxconv version                                    # tuxconv version 0.1.0-tuxconv

# a service source, end to end
./tuxconv convertgo path/to/SVC_FOO.pc -no-llm       # run 1: drafts a mapping, stops
./tuxconv convertgo path/to/SVC_FOO.pc -no-llm       # run 2: converts
```

The first run always stops after writing an editable mapping draft to
`mappings/` — that is the review step, not a failure. Generated code lands in
`conversion_logs/_staged/<service>/`; logs, per-file IR and the run ledger sit
beside it under `conversion_logs/`.

`-no-llm` keeps a run offline and reproducible. Drop it to let a configured
model fill the bodies that deterministic mode leaves as drafts or `TODO` seams.

## The pipeline

```
.pc → tsscan → pred → ir → flow → plan → convert → gen / pygen / csgen → gates
      parse   conds  extract slice mapping  bodies     emitters          validate
```

| Stage | What it does | Package |
|---|---|---|
| Parse | mask comments/strings, walk the AST into `SourceFacts` | `internal/tsscan` |
| Conditions | C-precedence boolean trees for branch conditions | `internal/pred` |
| Extract | query units, cursors, FML ops, tpcalls, defines → IR | `internal/ir` |
| Slice | scenarios per dispatch arm; mapping drafts | `internal/flow` |
| Model | shared service contract, SQL canonicalizer, language namers | `internal/contract`, `internal/sqltext`, `internal/namer` |
| Plan | decomposition plan + your endpoint mapping | `internal/plan` |
| Convert | deterministic body synthesis, LLM seam, retries | `internal/convert`, `internal/llm` |
| Emit | Go / Python / C# generators | `internal/gen`, `internal/pygen`, `internal/csgen` |
| Gates | gofmt/build/vet, SQL fidelity, structural checks | `internal/validate`, `internal/sqlchk`, `internal/cschk`, `internal/pychk` |
| Tests | generated assertion suites | `internal/testgen`, `internal/pygentest` |

Supporting packages: `internal/templates` (spec structs + the embedded `.tmpl`
files every emitter renders), `internal/config`, `internal/budget`,
`internal/inline`, `internal/ledger`, `internal/audit`, `internal/goast`,
`internal/gomockgen`, `internal/telemetry`.

## Repo map

```
cmd/tuxconv/   the CLI — one file per command (convert.go, convertcs.go, batchpy.go, …)
cmd/xtux/      debug-only inspection CLI; built with -tags tools
internal/      40+ packages, one concern each (table above)
configs/       .tuxconv.example.yaml — every knob, commented
web/           optional Next.js viewer over the CLI (see web/README.md)
prd/           the architecture sketch (convert-tux-to-go.excalidraw)
```

## Where to change what

| You want to change | Go to |
|---|---|
| the shape of generated Go | `internal/templates/templates/*.tmpl` (override per project with `templates.dir` / `-templates`) |
| generated Go logic | `internal/gen/` |
| Go naming and type projection | `internal/namer/` |
| Python batch output | `internal/pygen/`, `internal/pyplan/`, `internal/pystmt/` |
| C# output | `internal/csgen/`, `internal/csplan/`, `internal/cschk/` |
| what a command does, or a new flag | `cmd/tuxconv/<command>.go` |
| defaults and knobs | `configs/.tuxconv.example.yaml` + `internal/config/` |
| parsing behaviour | `internal/tsscan/` |
| the web viewer | `web/` |

## Reading order for newcomers

1. `README.md` — the what, the contracts kept, the fidelity guarantees.
2. `README-executable.md` — every command with a worked example.
3. `prd/convert-tux-to-go.excalidraw` — the architecture sketch.
4. `internal/ir/types.go` — the IR records every stage passes around.
5. `internal/templates/templates/` — the literal shape of what gets written.
6. `configs/.tuxconv.example.yaml` — the whole configuration surface in one file.

## Things that surprise people

- **Two-pass mapping.** `convertgo` and `convertcs` without a mapping draft one
  and stop. Re-run the same command to convert.
- **`-no-llm` is the reproducible posture.** No keys, no network, same bytes
  every run; `workers=1` byte-identity holds by construction.
- **The debug CLI needs a build tag.** `go run -tags tools ./cmd/xtux …`; it is
  deliberately excluded from normal builds.
- **Runtime directories are working state, not source.** `conversion_logs/`,
  `mappings/`, `python_out/` and `scenarios/` are rewritten per run.
