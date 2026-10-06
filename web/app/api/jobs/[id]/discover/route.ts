import { promises as fs } from "node:fs";
import { join } from "node:path";
import { NextResponse } from "next/server";
import { getJob, setJob, makeLogger } from "@/lib/jobs";
import { runTux } from "@/lib/tuxconv";
import { recordMetric } from "@/lib/metrics";

// POST /api/jobs/:id/discover { target: "go" | "cs", useLLM?: boolean }
// Step 1 of the mapping-first flow: runs the scan-then-tag draft pass.
// useLLM=false (default) appends -no-llm → deterministic names; useLLM=true
// omits it → AI naming when a key resolves, deterministic fallback
// otherwise (no key required — the run degrades, never fails). -target cs
// is deterministic-only throughout, so the flag is a no-op there.
export async function POST(req: Request, { params }: { params: { id: string } }) {
  const job = getJob(params.id);
  if (!job) return NextResponse.json({ error: "unknown job" }, { status: 404 });
  if (job.status === "running") return NextResponse.json({ error: "job is busy" }, { status: 409 });
  const body = (await req.json().catch(() => ({}))) as { target?: string; useLLM?: boolean };
  const target = body.target === "cs" ? "cs" : "go";
  const useLLM = body.useLLM === true;

  job.status = "running";
  job.error = undefined;
  setJob(job);
  const log = makeLogger(job);

  try {
    const outDir = join(job.dir, "mappings");
    let args: string[];
    if (target === "cs") {
      args = ["discover", job.sourcePath, "-out", outDir, "-target", "cs"];
    } else if (useLLM) {
      args = ["discover", job.sourcePath, "-out", outDir];
    } else {
      args = ["discover", job.sourcePath, "-out", outDir, "-no-llm"];
    }
    log(`$ discover target=${target} llm=${useLLM ? "on" : "off"}${target === "cs" ? " (cs is deterministic-only)" : ""}`);
    const r = await runTux(job.dir, args, log);
    if (r.code !== 0) throw new Error("discover failed — see logs");
    const names = await fs.readdir(outDir);
    const drafts = [];
    for (const n of names) {
      if (!/\.ya?ml$/i.test(n)) continue;
      const p = join(outDir, n);
      drafts.push({ path: p, content: await fs.readFile(p, "utf8") });
    }
    if (drafts.length === 0) throw new Error("discover wrote no drafts — see logs");
    drafts.sort((a, b) => a.path.localeCompare(b.path));
    job.drafts = drafts;
    job.mappingPath = drafts[0].path;
    job.mappingLLM = useLLM && target !== "cs";
    // Effective mode: the draft text is the ground truth. AI naming marks
    // its proposals `# ai-suggested`; the deterministic picker marks
    // `# deterministic`. An LLM-on run with zero ai-suggested lines means
    // the seam degraded (no key resolves in the server env) — surface that
    // instead of letting the toggle look broken.
    const hasAi = drafts.some((d) => d.content.includes("ai-suggested"));
    if (target === "cs") {
      job.mappingLLMEffective = false;
      job.mappingLLMNote = useLLM
        ? "C# drafts are deterministic-only — the LLM toggle is a no-op for C#."
        : "deterministic by design (C# drafts never call the LLM).";
    } else if (!useLLM) {
      job.mappingLLMEffective = false;
      job.mappingLLMNote = "deterministic by request (LLM off) — no key needed.";
    } else if (hasAi) {
      job.mappingLLMEffective = true;
      job.mappingLLMNote = "AI naming applied (# ai-suggested proposals).";
    } else {
      job.mappingLLMEffective = false;
      job.mappingLLMNote =
        "LLM requested but the draft is fully deterministic — no API key resolves in the server env (VLLM_API_KEY / OPENROUTER_API_KEY), so the seam fell back. Set a key, restart the web server, and re-draft. See Logs.";
    }
    job.status = "done";
    await recordMetric({ kind: "discover", job: job.name, target, files: drafts.length });
  } catch (e) {
    job.status = "error";
    job.error = e instanceof Error ? e.message : "discover failed";
  }
  setJob(job);
  return NextResponse.json({
    status: job.status,
    error: job.error,
    drafts: job.drafts,
    mappingPath: job.mappingPath,
    mappingLLM: job.mappingLLM,
    mappingLLMEffective: job.mappingLLMEffective,
    mappingLLMNote: job.mappingLLMNote,
  });
}
