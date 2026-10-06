# tux-migration-tool viewer — shadcn UI over the tuxconv CLI

Drag & drop `.pc` / `.pcf` files (multi-select, folder, or `.zip` — up to 50 files, 5MB each / 30MB
total) or pick a bundled sample and watch the batch break down:

Upload batch → Overview dashboard + Analyze triage (LOW/MEDIUM/HIGH effort) → IR tables → flow
coverage → dispatch-axis scenarios + scenarioFilter playground → editable mapping drafts → converted
tree (**Tux → Go · Python · C#**) → log-driven gentest (gap report + fixture preflight, generated tests,
nice names, verified snapshot) → live CLI logs.

The 6-step wizard (Upload → Overview → Scenarios → Mapping → Convert → Tests) collapses the old
11-tab sprawl: IR / Flow / Source live as Overview sub-tabs, Trace / Logs as Convert sub-tabs,
Metrics as a Tests sub-tab. Each step shows Back / Next with a blocked-step guard, and re-uploading
asks to confirm before replacing the current batch.

No database, no auth in the browser. Every run shells out to the
`tuxconv` CLI inside a disposable tmpdir (`TUXCONV_BIN` or `go run` fallback).
Each of Step 1 (mapping) and Step 2 (convert) has its own LLM toggle
(off = `-no-llm`, deterministic, no keys needed; on = LLM seam when a key
resolves, deterministic fallback otherwise). Jobs are in-memory tmpdir
snapshots — a server restart drops them (re-upload; runs are seconds), so
convert ships a **Download .zip** button for the durable copy.

## Run it

```sh
# Build the CLI once (preferred: instant runs, no `go run` latency):
go build -o bin/tuxconv ./cmd/tuxconv
export TUXCONV_BIN=$PWD/bin/tuxconv   # optional; else `go run` fallback

cd web
npm install
npm run dev     # http://localhost:3000
npm run build && npm run start   # production
```

Docker (multi-stage: Go binary + standalone Next):

```sh
docker build -f web/Dockerfile -t tux-web .
# Keep it loopback-only — no LAN exposure:
docker run -p 127.0.0.1:3000:3000 tux-web
```

## Privacy — local-only, no phone-home

This viewer is a local tool. Runtime network behavior, verified:

- Browser → only same-origin `/api/*` (upload, discover, convert, …).
  No analytics SDK, no tracking pixel, no Google Fonts, no CDN scripts,
  no Vercel Analytics, no `next/image` remotes. `grep -r "https://"`
  in `web/{app,components,hooks,lib}` hits nothing but comments.
- `tuxconv` CLI → no network in deterministic mode: every `-no-llm` call
  stays local. LLM-on calls (when a key resolves) go to the configured
  `apiBase` only — same egress contract as the CLI. The Go
  `internal/telemetry` package is local `slog` file logging
  (`conversion_logs/logs/`) despite the name.
- Uploaded `.pc` files and attached runtime logs stay in disposable tmpdirs
  (`tux-web-*`) on your machine; nothing leaves it. Logs are capped at 5MB,
  never bundled into the Download .zip, and only their numeric fixture
  counts reach the master metrics log.

Next.js anonymous telemetry (build counts, not your code) is disabled
three ways — any one suffices, all three are set so clones/CI/containers
are covered:

1. `NEXT_TELEMETRY_DISABLED=1` in `package.json` scripts, `.env.example`,
   and `Dockerfile` (`ENV` + `npx next telemetry disable` at build).
2. Machine-wide opt-out: `npx next telemetry disable`
   (already run here — `npx next telemetry status` → Disabled).
3. Verify anytime: `npm run telemetry:status`.

### Alternatives / mitigations

- **Fully offline after install:** `npm ci --offline` works once the
  cache is warm; `next build` / `next start` need no network. Disconnect
  and the viewer still runs — try it before a demo.
- **No-Next alternative:** the viewer is a thin shell over `tuxconv`.
  Every button maps to one CLI command (`extract`, `flow -scenarios`,
  `discover -list-axes/-filter`, `convertgo/convertbatchpy/convertcs`,
  `gentest`), so a locked-down review can skip the browser entirely.
- **Air-gapped Docker:** multi-stage build needs the network once;
  the runtime image makes no egress. Run with `--network none`
  (after pulling base images) to prove it.

### Dependency risk

Direct deps are minimal and reputable (`npm ls --depth=0`):
`next`, `react`, `react-dom`, six `@radix-ui/*` primitives,
`class-variance-authority`, `clsx`, `tailwind-merge`, `lucide-react`
(icons), plus dev-only `tailwindcss`, `postcss`, `autoprefixer`,
`typescript`, `@types/*`. No analytics, no axios/fetch wrappers, no
obfuscated bundles. `package-lock.json` contains zero `postinstall`
scripts; the only install script is `fsevents` (macOS file-watching,
pulled by Next itself).

`npm audit` on Next 14.2.35 reports GHSA advisories (cache poisoning,
middleware/i18n bypass, Server-Action SSRF/DoS, Image-Optimization RCE,
postcss source-map reads). Exposure here is minimal: the server binds
`127.0.0.1` (dev/start scripts), uses no middleware/i18n/Server Actions/
`next/image` optimization, and only serves trusted-local route handlers.
Mitigations: keep it on loopback, don't expose it to untrusted networks,
and upgrade Next when a non-breaking patched 14.x lands
(`npm audit fix --force` would jump to Next 16 — breaking — so it is
intentionally not applied).

## LLM toggles — requested vs effective

Step 1 (mapping) and Step 2 (convert) each have an LLM toggle:

- **Off** appends `-no-llm` — deterministic drafts (`# deterministic — edit
  freely`, `tuxconv:TODO` seams), no key needed, no network.
- **On** omits `-no-llm` — the LLM seam runs *when a key resolves*,
  deterministic fallback otherwise. No key is ever required; a run never
  hard-fails for lack of one.

Why LLM-on can still say "deterministic": the web server runs `tuxconv`
inside a disposable job tmpdir with **no `.tuxconv.yaml`** of its own, so
without a server config the stock defaults apply — profile `onprem-vllm`
(key `$VLLM_API_KEY`), alternative `local-dev-openrouter` (key
`$OPENROUTER_API_KEY`). Unless the *server* process was started with one of
those set, `resolveLLMClient` returns nil and the seam degrades. Two fixes
(env-first, same as the CLI — pick one):

```sh
export VLLM_API_KEY=...        # or OPENROUTER_API_KEY=...
# restart the web server so Next picks it up, then re-draft / re-convert
```

```sh
# ...or share the CLI's yaml (models[].apiKey literal included):
export TUXCONV_CONFIG=$PWD/.tuxconv.yaml   # gitignored working copy only, never commit
# restart the web server; every tuxconv call it spawns gets -config <that path>
```

With `TUXCONV_CONFIG` set, browser runs resolve keys exactly like the CLI
(env var wins, yaml literal is the fallback), and `GET /api/llm-status` /
`GET /api/db-status` report yaml-aware presence (booleans only, never
values). Without it, only server-env keys count.

The UI reports **requested vs effective** so the fallback never looks like
a broken toggle:

- `GET /api/llm-status` — key presence (booleans only, never values); the
  header shows `llm key ok/missing` and each toggle warns `no key — expect
  fallback` while on.
- Discover counts the draft's origin markers (`N ai-suggested ·
  M deterministic`) and stores `mappingLLMEffective` + `mappingLLMNote`.
- Convert parses the CLI's `N llm calls` summary tail and stores
  `llmEffective` + `llmNote` + `llmCalls` (the summary line itself gains
  `llm: requested=… effective=…`).
- C# drafts are deterministic-only by design — the mapping toggle is a
  documented no-op there.

## GT-7 — runtime-log fixtures in the Tests step

The Tests step exposes the shipped GT-7 CLI surface end-to-end:

- **Attach a runtime log** — drag the edited app's `.txt`/`.log` (or paste it).
  It is stored at `<job>/gentest/runtime.log`, capped at 5MB, and used with
  `gentest -log-file`. **Gap report** then prints a *fixture preflight* before
  anything is generated: `N from log · M assumed`, trace/line/warning counts.
- **Fixture provenance** — after a generate run the panel parses the newest
  `conversion_logs/audit/<run-id>/gentest_summary.json` into a filterable
  `service · layer · func · source` table (`log <short-id>` vs `assumed`),
  plus the gate lines. Generated files carry `// fixture: …` comments and open
  in an inline viewer (Convert-tree files and snapshot files alike).
- **Nice names** — `--nice-names` is available when the LLM toggle is on;
  requested-vs-effective is reported (no key → names stay deterministic).
  The log path itself needs no LLM: controller fixtures are derived from the
  trace, so a keyless run is still log-driven.
- **Verified snapshot** — `-out <job>/go-gentest` stages non-test sources +
  generated tests and runs the full `go test` gate inside the snapshot; the
  converted tree is untouched. Download it via the summary strip
  (`GET /api/jobs/:id/archive?tree=gentest`). On module-less converted trees
  the CLI records `gate: skipped (… outside any Go module)` visibly instead of
  pretending the gate ran.
- **In-place (default)** — without the snapshot checkbox the browser passes
  `-in-place`, so tests land next to the converted code and appear in the
  Convert tree even for module-less trees.

Privacy: a runtime log can carry request bodies and internal endpoints, so it
stays in the disposable job tmpdir, is never echoed into
`conversion_logs/web-metrics.jsonl` (only numeric fixture counts are), and is
never bundled into the Download .zip.

## Where converted files live + Download .zip

Per job (server-local, disposable):

```text
$TMPDIR/tux-web-*/        # job.dir — vanishes on server restart
  input/<name>.pc         # your upload
  ir.json / flow.json     # deterministic parse artifacts
  mappings/*.mapping.yaml # drafts (Step 1 saves in place)
  scenarios/              # flow -scenarios artifacts
  go/ | py/ | cs/         # converted tree (Step 2; -base/-out root)
  logs/                   # tuxconv -log-dir output
  job.json                # job snapshot (log tail capped at 500)
```

The Convert panel shows the tree's absolute `root` path and the per-file
viewer (`files.tsx`) keeps per-file Copy/Download. For the durable copy:

- `GET /api/jobs/:id/archive` — streams `<name>-<target>.zip` containing
  `<target>/…` (the converted tree), `mapping/<draft>.yaml` (when saved),
  and `tux-migration-tool-summary.txt` (target, file count, requested/effective LLM
  mode). Pure-Node STORE zip (no compression, no extra deps, no `zip`
  binary needed): ≤500 files, ≤1MB per file, ≤50MB total; oversized files
  are skipped and named in the summary.
- The Convert panel's **Download .zip** button links straight there.

## Architecture (frontend)

Clean separation — routes stay thin, logic lives in `lib`, state in `hooks`:

```text
web/
  app/
    page.tsx              # composition only: header + stepper + tabs + panels
    layout.tsx            # metadata + TooltipProvider
    api/
      jobs/route.ts                 # POST upload → extract + flow + flow.json
      jobs/[id]/scenarios/route.ts  # POST flow -scenarios + discover -list-axes
      jobs/[id]/filter-preview/     # POST discover -filter "<expr>" (read-only)
      jobs/[id]/discover/           # POST draft Go / C# mappings
      jobs/[id]/mapping/            # PUT save edited draft
      jobs/[id]/convert/            # POST convertgo / convertbatchpy / convertcs
      jobs/[id]/lineage/            # GET source→output trace (this part became that part)
      jobs/[id]/gentest/            # POST gap report / generate (GT-7 options)
      jobs/[id]/gentest/log/        # POST attach / DELETE runtime log (GT-7)
      jobs/[id]/files|logs|route    # file read, log tail, job fetch
      jobs/[id]/archive/            # GET whole converted tree + mapping as .zip
      llm-status/route.ts           # GET LLM key presence (booleans only)
      db-status/route.ts            # GET Oracle DSN presence (booleans only)
      samples{,/load}/route.ts      # curated fixtures for one-click demos
  components/
    ui/                   # shadcn primitives (Radix): button, tabs, select,
                          # progress, tooltip, table, card, badge, alert…
    site-header.tsx       # branding + job status
    pipeline-steps.tsx    # Upload → Overview → Scenarios → Mapping → Convert → Tests
    dropzone.tsx          # validated drag & drop (.pc/.pcf, 5MB)
    samples-panel.tsx     # one-click fixtures
    overview-dashboard.tsx# KPI cards + SVG donut/bars + coverage
    ir-explorer.tsx       # searchable condition/query/function/FML tables
    flow-explorer.tsx     # per-function coverage + hints + raw text
    scenario-explorer.tsx # axis registry, slice picker, shared-md, filter playground
    mapping-editor.tsx    # draft Go/C#, edit, save & continue
    convert-panel.tsx     # Tux→Go/Python/C# target cards + file tree + code view + provenance banner
    lineage-explorer.tsx  # Trace tab: pick a source part → see every file it became, with evidence
    tests-panel.tsx       # GT-7 Tests step: log attach + fixture preflight,
                          # generate options, provenance table, gates, viewer
    files.tsx             # filterable file tree + copy/download code view
  hooks/use-job.ts        # job + log polling
  hooks/use-lineage.ts    # source→output trace fetch (refetches per converted tree)
  lib/
    api-client.ts         # single home for browser → API calls
    jobs.ts               # Job / FlowReport / ScenarioBundle types + store
    lineage.ts            # evidence-based IR→file matcher (exact/derived/related)
    llm-status.ts         # server-env + TUXCONV_CONFIG yaml key presence (no values)
    targets.ts            # Tux→Go/Python/C# catalog
    ir.ts                 # case-tolerant IR accessors
    server-config.ts      # TUXCONV_CONFIG resolution + -config injection + yaml snapshot
    tuxconv.ts            # CLI spawn helper (injects server -config)
```

Every button runs something real: upload, sample, draft, build scenarios,
filter preview, save mapping, convert (per target), attach/clear runtime log,
gap report (with fixture preflight), generate (log fixtures / nice names /
verified snapshot), file open, copy/download, download-all-.zip, log tail.
Busy states disable actions; errors surface inline; draft-and-stop converts
bounce you to the Mapping tab. Requested-vs-effective LLM badges (plus
`/api/llm-status`) explain every deterministic fallback.
