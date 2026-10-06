import { join } from "node:path";
import { NextResponse } from "next/server";
import { getJob, setJob, makeLogger } from "@/lib/jobs";
import { listFilesRecursive, runTux } from "@/lib/tuxconv";
import { countQueries, recordMetric } from "@/lib/metrics";

type Target = "go" | "py" | "cs";

// POST /api/jobs/:id/convert { target, mappingPath?, useLLM? }
// Step 2 of the mapping-first flow: Go/CS require a reviewed mapping —
// without one the run drafts-and-stops and returns the fresh drafts instead
// of a tree (the UI guides this as Step 1 → Step 2).
// useLLM=false (default) appends -no-llm → deterministic bodies with
// SQL-fidelity gates; useLLM=true omits it → LLM seam when a key resolves,
// deterministic fallback otherwise (no key required).
export async function POST(req: Request, { params }: { params: { id: string } }) {
  const job = getJob(params.id);
  if (!job) return NextResponse.json({ error: "unknown job" }, { status: 404 });
  if (job.status === "running") return NextResponse.json({ error: "job is busy" }, { status: 409 });
  const body = (await req.json().catch(() => ({}))) as { target?: Target; mappingPath?: string; useLLM?: boolean };
  const target: Target = body.target === "py" || body.target === "cs" ? body.target : "go";
  const useLLM = body.useLLM === true;
  // Batch jobs: the mappings dir carries one draft per source file, so pass
  // the dir (CLI accepts yaml|dir) instead of a single draft.
  const { promises: fs0 } = await import("node:fs");
  let mapping = body.mappingPath ?? job.mappingPath;
  if (job.isBatch && (target === "go" || target === "cs")) {
    try {
      const names = await fs0.readdir(join(job.dir, "mappings"));
      if (names.some((n) => /\.ya?ml$/i.test(n))) mapping = join(job.dir, "mappings");
    } catch {
      /* fall back to single mapping */
    }
  }
  const noLLMFlag = useLLM ? [] : ["-no-llm"];

  job.status = "running";
  job.error = undefined;
  job.converted = undefined;
  setJob(job);
  const log = makeLogger(job);

  try {
    let root = "";
    let args: string[];
    if (target === "go") {
      root = join(job.dir, "go");
      args = ["convertgo", job.sourcePath, ...noLLMFlag, "-base", root];
      if (mapping) args.push("-mapping", mapping);
    } else if (target === "py") {
      root = join(job.dir, "py");
      args = ["convertbatchpy", job.sourcePath, ...noLLMFlag, "-out", root];
    } else {
      root = join(job.dir, "cs");
      args = ["convertcs", job.sourcePath, ...noLLMFlag, "-out", root];
      if (mapping) args.push("-mapping", mapping);
    }
    log(`$ convert target=${target} llm=${useLLM ? "on" : "off"} mapping=${mapping ?? "(none — draft-and-stop)"}`);
    const r = await runTux(job.dir, args, log);
    const combined = `${r.stdout}\n${r.stderr}`;
    // Effective mode: the CLI prints "N llm calls" per service summary line
    // (printServiceSummary). LLM-on with zero calls anywhere means the seam
    // degraded (no key resolves) or nothing needed it — never report the
    // requested flag as the outcome.
    let llmCalls = 0;
    const llmRe = /(\d+)\s+llm calls?/gi;
    let m: RegExpExecArray | null;
    while ((m = llmRe.exec(combined)) !== null) {
      const n = Number(m[1]);
      if (Number.isFinite(n) && n > llmCalls) llmCalls = n;
    }
    const llmEffective = useLLM && llmCalls > 0;
    const llmNote = !useLLM
      ? "deterministic by request (LLM off — -no-llm), no key needed."
      : llmEffective
        ? `${llmCalls} LLM call(s) — AI seam filled bodies.`
        : target === "py"
          ? "LLM requested but 0 calls made — simple-shape Python is fully deterministic, or no API key resolves (VLLM_API_KEY / OPENROUTER_API_KEY). See Logs."
          : "LLM requested but 0 calls made — no API key resolves in the server env (VLLM_API_KEY / OPENROUTER_API_KEY), so the run fell back to deterministic drafts (tuxconv:TODO seams). Set a key, restart the web server, and re-convert. See Logs.";
    const files = await listFilesRecursive(root);
    if (files.length === 0) {
      // Draft-and-stop (or a failure): surface fresh drafts when present.
      const { promises: fs } = await import("node:fs");
      const drafts = [];
      try {
        for (const n of await fs.readdir(join(job.dir, "mappings"))) {
          if (!/\.ya?ml$/i.test(n)) continue;
          const p = join(job.dir, "mappings", n);
          drafts.push({ path: p, content: await fs.readFile(p, "utf8") });
        }
      } catch {
        /* no mappings dir */
      }
      if (drafts.length > 0) {
        job.drafts = drafts;
        job.mappingPath = drafts[0].path;
        // Draft-and-stop drafts obey the same origin marks as discover.
        const hasAi = drafts.some((d) => d.content.includes("ai-suggested"));
        job.mappingLLM = useLLM;
        job.mappingLLMEffective = useLLM && hasAi;
        job.mappingLLMNote = !useLLM
          ? "deterministic by request (LLM off) — no key needed."
          : hasAi
            ? "AI naming applied (# ai-suggested proposals)."
            : "LLM requested but the draft is fully deterministic — no API key resolves in the server env (VLLM_API_KEY / OPENROUTER_API_KEY). Set a key, restart, and re-run. See Logs.";
      }
      if (r.code !== 0 && drafts.length === 0) throw new Error("convert failed — see logs");
      job.status = "done";
      setJob(job);
      return NextResponse.json({
        status: job.status,
        drafts: job.drafts,
        mappingPath: job.mappingPath,
        note: "no mapping yet — review the draft, then convert again",
      });
    }
    const tail = r.stdout.trim().split("\n").slice(-3).join("\n");
    job.converted = {
      target,
      root,
      files: files.map((f) => f.slice(root.length + 1)),
      summary: `${tail}\nllm: requested=${useLLM ? "on" : "off"} effective=${llmEffective ? "on" : "off"} (${llmCalls} calls)`,
      llm: useLLM,
      llmEffective,
      llmNote,
      llmCalls,
    };
    job.status = "done";
    await recordMetric({
      kind: "convert",
      job: job.name,
      target,
      queries: countQueries(job.ir),
      files: job.converted.files.length,
    });
  } catch (e) {
    job.status = "error";
    job.error = e instanceof Error ? e.message : "convert failed";
  }
  setJob(job);
  return NextResponse.json({ status: job.status, error: job.error, converted: job.converted });
}
