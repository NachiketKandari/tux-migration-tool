import { promises as fs } from "node:fs";
import { basename, join } from "node:path";
import { NextResponse } from "next/server";
import { getJob, setJob, makeLogger } from "@/lib/jobs";
import { parseTriageCsv } from "@/lib/analysis";
import { runTux } from "@/lib/tuxconv";

// POST /api/jobs/:id/analyze — triage rubric over the job's sources.
// Deterministic, zero LLM calls. Re-runnable; stores rows + CSV on the job.
export async function POST(_req: Request, { params }: { params: { id: string } }) {
  const job = getJob(params.id);
  if (!job) return NextResponse.json({ error: "unknown job" }, { status: 404 });
  if (job.status === "running") return NextResponse.json({ error: "job is busy" }, { status: 409 });

  job.status = "running";
  job.error = undefined;
  setJob(job);
  const log = makeLogger(job);

  try {
    const csvPath = join(job.dir, "triage.csv");
    const r = await runTux(job.dir, ["analyze", job.sourcePath, "-csv", csvPath], log);
    if (r.code !== 0) throw new Error("analyze failed — see logs");
    const csv = await fs.readFile(csvPath, "utf8");
    job.analysisCsv = csv.length > 256 * 1024 ? csv.slice(0, 256 * 1024) + "\n…[truncated]" : csv;
    job.analysis = parseTriageCsv(csv).map((row) => ({ ...row, file: basename(row.file) }));
    job.status = "done";
  } catch (e) {
    job.status = "error";
    job.error = e instanceof Error ? e.message : "analyze failed";
  }
  setJob(job);
  return NextResponse.json({ status: job.status, error: job.error, analysis: job.analysis ?? null });
}

export async function GET(_req: Request, { params }: { params: { id: string } }) {
  const job = getJob(params.id);
  if (!job) return NextResponse.json({ error: "unknown job" }, { status: 404 });
  return NextResponse.json({ status: job.status, analysis: job.analysis ?? null, csv: job.analysisCsv ?? null });
}
