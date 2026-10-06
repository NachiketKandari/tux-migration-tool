import { promises as fs } from "node:fs";
import { join, basename } from "node:path";
import { NextResponse } from "next/server";
import { getJob, setJob, makeLogger } from "@/lib/jobs";
import { runTux } from "@/lib/tuxconv";
import { recordMetric } from "@/lib/metrics";


// POST /api/jobs/:id/scenarios — build the dispatch-axis scenario explorer
// bundle: `flow -scenarios` artifacts + `discover -list-axes` registry text.
// Read-only w.r.t. the source; artifacts land under <job>/scenarios/.
export async function POST(_req: Request, { params }: { params: { id: string } }) {
  const job = getJob(params.id);
  if (!job) return NextResponse.json({ error: "unknown job" }, { status: 404 });
  if (job.status === "running") return NextResponse.json({ error: "job is busy" }, { status: 409 });

  job.status = "running";
  job.error = undefined;
  setJob(job);
  const log = makeLogger(job);

  try {
    const scenDir = join(job.dir, "scenarios");

    const sc = await runTux(job.dir, ["flow", job.sourcePath, "-scenarios", "-scenarios-dir", scenDir], log);
    const scenStdout = (sc.stdout || "") + (sc.stderr ? `\n${sc.stderr}` : "");

    const ax = await runTux(job.dir, ["discover", job.sourcePath, "-list-axes"], log);
    const axesText = ax.stdout || ax.stderr || "(no axes output)";

    let names: string[] = [];
    try {
      names = await fs.readdir(scenDir);
    } catch {
      names = [];
    }
    names.sort();

    const artifacts: { name: string; content: string }[] = [];
    let sharedMd: string | undefined;
    let sharedJson: unknown | undefined;
    let axesMd: string | undefined;

    for (const n of names) {
      const p = join(scenDir, n);
      try {
        const st = await fs.stat(p);
        if (!st.isFile()) continue;
        const content = await fs.readFile(p, "utf8");
        if (/\.shared\.md$/i.test(n)) sharedMd = content;
        else if (/\.shared\.json$/i.test(n)) {
          try {
            sharedJson = JSON.parse(content);
          } catch {
            sharedJson = { raw: content.slice(0, 8000) };
          }
        } else if (/\.axes\.md$/i.test(n)) axesMd = content;
        else if (/\.pc$/i.test(n)) artifacts.push({ name: n, content });
        // .axes.json is machine data — surfaced via axesText; skip to keep the bundle lean.
      } catch {
        continue;
      }
    }

    const axisNone = /axis:\s*none|scenarios:\s*0/i.test(scenStdout);
    job.scenarios = {
      axesText,
      axesMd,
      sharedMd,
      sharedJson,
      artifacts,
      note: axisNone
        ? "No dispatch spine detected — this file folds as one scenario. Use Discover for condition drafts."
        : artifacts.length === 0 && !sharedMd
          ? "Scenario run produced no slice files — see logs for the axis summary."
          : undefined,
    };
    void basename;
    job.status = "done";
    await recordMetric({ kind: "scenarios", job: job.name, scenarios: artifacts.length });
  } catch (e) {
    job.status = "error";
    job.error = e instanceof Error ? e.message : "scenarios failed";
  }
  setJob(job);
  return NextResponse.json({ status: job.status, error: job.error, scenarios: job.scenarios });
}

// GET returns the cached bundle without re-running the CLI.
export async function GET(_req: Request, { params }: { params: { id: string } }) {
  const job = getJob(params.id);
  if (!job) return NextResponse.json({ error: "unknown job" }, { status: 404 });
  return NextResponse.json({ status: job.status, scenarios: job.scenarios ?? null });
}
