import { promises as fs } from "node:fs";
import { isAbsolute, join, relative } from "node:path";
import { NextResponse } from "next/server";
import {
  getJob,
  setJob,
  makeLogger,
  type GentestFileEntry,
  type GentestPreflight,
  type GentestSummary,
} from "@/lib/jobs";
import { listFilesRecursive, runTux } from "@/lib/tuxconv";
import { recordMetric } from "@/lib/metrics";

// POST /api/jobs/:id/gentest
//   { mode: "check" | "generate", useLLM?, niceNames?, staged?, withLog? }
//
// Runs against the converted Go tree. Gap report = `-check-only` (plus
// `-log-file` when a runtime log is attached → GT-7 fixture preflight).
// Generate = template-deterministic suites; with a log attached the
// controller fixtures come from real values, `niceNames` adds the
// literals-only LLM polish (needs a key), and `staged` writes a complete
// `-out` snapshot (sources + tests, full `go test` gate) without touching
// the converted tree.
//
// After a generate run the newest conversion_logs/audit/<run-id>/
// gentest_summary.json is parsed back into the job, so the UI can show
// per-method fixture provenance, gates, and written files.

function parsePreflight(out: string): GentestPreflight | undefined {
  const fixtures = out.match(/log: fixtures — (\d+) scanned functions with log values, (\d+) assumed \(of (\d+)\)/);
  if (!fixtures) return undefined;
  const stats = out.match(/log: (.+) — (\d+) lines, (\d+) traces \((\d+) complete\), (\d+) warnings, (\d+) requestID:null skipped, (\d+) stack lines/);
  return {
    path: stats?.[1] ?? "",
    lines: stats ? Number(stats[2]) : 0,
    traces: stats ? Number(stats[3]) : 0,
    complete: stats ? Number(stats[4]) : 0,
    warnings: stats ? Number(stats[5]) : 0,
    fromLog: Number(fixtures[1]),
    assumed: Number(fixtures[2]),
  };
}

function relInside(base: string, abs: string): string | null {
  const r = relative(base, abs);
  if (!r || r.startsWith("..") || isAbsolute(r)) return null;
  return r.split("\\").join("/");
}

// readSummary finds the newest gentest_summary.json the run just wrote and
// resolves every absolute path against the target tree / -out snapshot.
async function readSummary(
  jobDir: string,
  targetRoot: string,
  outRoot: string | null
): Promise<GentestSummary | undefined> {
  const auditRoot = join(jobDir, "conversion_logs", "audit");
  let newest: { path: string; mtime: number } | undefined;
  try {
    for (const e of await fs.readdir(auditRoot, { withFileTypes: true })) {
      if (!e.isDirectory()) continue;
      const p = join(auditRoot, e.name, "gentest_summary.json");
      try {
        const st = await fs.stat(p);
        if (!newest || st.mtimeMs > newest.mtime) newest = { path: p, mtime: st.mtimeMs };
      } catch {
        /* not this run's dir */
      }
    }
  } catch {
    return undefined;
  }
  if (!newest) return undefined;

  type RawFixture = { service?: string; layer?: string; func?: string; source?: string };
  type RawSummary = {
    fixtures?: RawFixture[];
    gates?: string[];
    tests_failed?: boolean;
    llm_calls?: number;
    files?: string[];
    staged?: string[];
  };
  let raw: RawSummary;
  try {
    raw = JSON.parse(await fs.readFile(newest.path, "utf8")) as RawSummary;
  } catch {
    return undefined;
  }

  const place = (abs: string): GentestFileEntry => {
    const inTarget = relInside(targetRoot, abs);
    if (inTarget) return { path: inTarget, tree: "target" };
    if (outRoot) {
      const inOut = relInside(outRoot, abs);
      if (inOut) return { path: inOut, tree: "gentest" };
    }
    return { path: relInside(jobDir, abs) ?? abs, tree: outRoot ? "gentest" : "target" };
  };

  const fixtures = (raw.fixtures ?? []).map((f) => ({
    service: f.service ?? "",
    layer: f.layer ?? "",
    func: f.func ?? "",
    source: f.source ?? "assumed",
  }));
  // Module-less trees can stage several same-named files at one flat path
  // (the CLI overwrites; last wins) — show each path once.
  const dedupe = (entries: GentestFileEntry[]): GentestFileEntry[] => {
    const seen = new Set<string>();
    return entries.filter((e) => {
      const key = `${e.tree}:${e.path}`;
      if (seen.has(key)) return false;
      seen.add(key);
      return true;
    });
  };
  return {
    fixtures,
    gates: raw.gates ?? [],
    testsFailed: raw.tests_failed === true,
    llmCalls: typeof raw.llm_calls === "number" ? raw.llm_calls : 0,
    files: dedupe((raw.files ?? []).map(place)),
    staged: dedupe((raw.staged ?? []).map(place)).map((f) => f.path),
    fromLog: fixtures.filter((f) => f.source !== "assumed").length,
    assumed: fixtures.filter((f) => f.source === "assumed").length,
  };
}

function countLLMCalls(out: string): number {
  let calls = 0;
  const re = /(\d+)\s+llm calls?/gi;
  let m: RegExpExecArray | null;
  while ((m = re.exec(out)) !== null) {
    const n = Number(m[1]);
    if (Number.isFinite(n) && n > calls) calls = n;
  }
  return calls;
}

export async function POST(req: Request, { params }: { params: { id: string } }) {
  const job = getJob(params.id);
  if (!job) return NextResponse.json({ error: "unknown job" }, { status: 404 });
  if (job.status === "running") return NextResponse.json({ error: "job is busy" }, { status: 409 });
  if (!job.converted || job.converted.target !== "go") {
    return NextResponse.json({ error: "convert a Go tree first" }, { status: 400 });
  }
  const body = (await req.json().catch(() => ({}))) as {
    mode?: string;
    useLLM?: boolean;
    niceNames?: boolean;
    staged?: boolean;
    withLog?: boolean;
  };
  const mode = body.mode === "generate" ? "generate" : "check";
  const useLLM = body.useLLM === true;
  const niceNames = body.niceNames === true && useLLM;
  const staged = body.staged === true;
  // The log rides along whenever one is attached unless the caller opts out.
  const withLog = body.withLog !== false && !!job.gentestLog;
  const logFile = join(job.dir, "gentest", "runtime.log");

  job.status = "running";
  job.error = undefined;
  setJob(job);
  const log = makeLogger(job);

  try {
    const root = job.converted.root;
    if (mode === "check") {
      const args = ["gentest", root, "-check-only"];
      if (withLog) args.push("-log-file", logFile);
      log(`$ gentest check-only log=${withLog ? "on" : "off"}`);
      const r = await runTux(job.dir, args, log);
      job.gentestGap = r.stdout || r.stderr;
      job.gentestPreflight = withLog ? parsePreflight(`${r.stdout}\n${r.stderr}`) : undefined;
    } else {
      const args = ["gentest", root];
      if (!useLLM) args.push("-no-llm");
      if (niceNames) args.push("--nice-names");
      if (withLog) args.push("-log-file", logFile);
      let outRoot: string | null = null;
      if (staged) {
        outRoot = join(job.dir, "go-gentest");
        args.push("-out", outRoot);
      } else {
        // Write next to the converted code even for module-less trees:
        // without -in-place those fall back to paths.staged and disappear
        // from the Convert tree. Module-ful trees resolve the same folder.
        args.push("-in-place");
      }
      log(
        `$ gentest llm=${useLLM ? "on" : "off"} nice-names=${niceNames ? "on" : "off"} log=${withLog ? "on" : "off"} out=${staged ? "snapshot" : "in-place"}`
      );
      const r = await runTux(job.dir, args, log);
      if (r.code !== 0) throw new Error("gentest failed — see logs");

      const summary = await readSummary(job.dir, root, outRoot);
      const combined = `${r.stdout}\n${r.stderr}`;
      const llmCalls = summary?.llmCalls ?? countLLMCalls(combined);
      const niceNamesSkipped = /nice-names: no LLM client available/.test(combined);
      job.gentestSummary = summary;
      job.gentestNiceNames = niceNames;
      job.gentestLLM = useLLM;
      job.gentestLLMEffective = llmCalls > 0;
      job.gentestLLMNote = !useLLM
        ? "deterministic by request (LLM off — -no-llm), no key needed."
        : llmCalls > 0
          ? `${llmCalls} LLM call(s) — ${niceNames ? "nice names + LLM seam" : "LLM seam"} applied.`
          : niceNamesSkipped
            ? "LLM requested but no key resolves — nice names skipped, tests stay deterministic (log/AST)."
            : "LLM requested but 0 calls made — the log/AST path covered everything deterministically.";
      job.gentestOutRoot = outRoot ?? undefined;

      const all = await listFilesRecursive(root);
      if (staged) {
        job.gentestFiles = (summary?.files ?? [])
          .filter((f) => f.path.endsWith("_test.go"))
          .map((f) => f.path);
      } else {
        job.gentestFiles = all.filter((f) => f.endsWith("_test.go")).map((f) => f.slice(root.length + 1));
        // Refresh the converted tree listing (new _test.go files landed).
        if (job.converted) {
          job.converted.files = all.map((f) => f.slice(root.length + 1));
        }
      }
      const tail = r.stdout.trim().split("\n").slice(-6).join("\n");
      job.gentestGap = tail;
      await recordMetric({
        kind: "gentest",
        job: job.name,
        target: "go",
        tests: job.gentestFiles.length,
        files: job.gentestFiles.length,
        fixturesFromLog: summary?.fromLog,
        fixturesAssumed: summary?.assumed,
        niceNames: niceNames || undefined,
        staged: staged || undefined,
      });
    }
    job.status = "done";
  } catch (e) {
    job.status = "error";
    job.error = e instanceof Error ? e.message : "gentest failed";
  }
  setJob(job);
  return NextResponse.json({
    status: job.status,
    error: job.error,
    gap: job.gentestGap,
    files: job.gentestFiles,
    preflight: job.gentestPreflight,
    summary: job.gentestSummary,
  });
}
