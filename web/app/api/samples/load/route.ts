import { promises as fs } from "node:fs";
import { basename, join } from "node:path";
import { NextResponse } from "next/server";
import { randomUUID } from "node:crypto";
import { setJob, makeLogger, type Job } from "@/lib/jobs";
import { coerceFlowReport } from "@/lib/flow";
import { parseTriageCsv } from "@/lib/analysis";
import { countQueries, recordMetric } from "@/lib/metrics";
import { newJobDir, runTux, REPO_ROOT } from "@/lib/tuxconv";

// POST /api/samples/load { path } — ingest a local .pc source as a job.
export async function POST(req: Request) {
  const body = (await req.json().catch(() => ({}))) as { path?: string };
  if (!body.path || !/^[\w./-]+\.pc$/i.test(body.path) || body.path.includes("..")) {
    return NextResponse.json({ error: "invalid source path" }, { status: 400 });
  }
  let src: Buffer;
  try {
    src = await fs.readFile(join(REPO_ROOT, body.path));
  } catch {
    return NextResponse.json({ error: "sample unavailable" }, { status: 404 });
  }
  const name = body.path.split("/").pop() ?? "sample.pc";
  const dir = await newJobDir();
  const inputDir = join(dir, "input");
  await fs.mkdir(inputDir, { recursive: true });
  const sourcePath = join(inputDir, name);
  await fs.writeFile(sourcePath, src);

  const job: Job = { id: randomUUID().slice(0, 8), name, dir, sourcePath, inputFiles: [name], isBatch: false, status: "running", logs: [] };
  setJob(job);
  const log = makeLogger(job);
  try {
    const irPath = join(dir, "ir.json");
    const ex = await runTux(dir, ["extract", sourcePath, "-out", irPath], log);
    if (ex.code !== 0) throw new Error("extract failed — see logs");
    job.ir = JSON.parse(await fs.readFile(irPath, "utf8"));
    const fl = await runTux(dir, ["flow", sourcePath], log);
    job.flowText = fl.stdout || "(no flow output)";
    try {
      const flowPath = join(dir, "flow.json");
      const fr = await runTux(dir, ["flow", sourcePath, "-out", flowPath], log);
      if (fr.code === 0) {
        const raw = JSON.parse(await fs.readFile(flowPath, "utf8"));
        job.flowReport = coerceFlowReport(raw, sourcePath);
      }
    } catch {
      /* text fallback */
    }
    try {
      const csvPath = join(dir, "triage.csv");
      const an = await runTux(dir, ["analyze", sourcePath, "-csv", csvPath], log);
      if (an.code === 0) {
        const csv = await fs.readFile(csvPath, "utf8");
        job.analysisCsv = csv;
        job.analysis = parseTriageCsv(csv).map((r) => ({ ...r, file: basename(r.file) }));
      }
    } catch {
      /* advisory */
    }
    try {
      const s = await fs.readFile(sourcePath, "utf8");
      job.sourcePreview = s.length > 24 * 1024 ? s.slice(0, 24 * 1024) + "\n…[truncated]" : s;
    } catch {
      /* optional */
    }
    job.status = "done";
    await recordMetric({ kind: "parse", job: job.name, queries: countQueries(job.ir), note: "sample" });
  } catch (e) {
    job.status = "error";
    job.error = e instanceof Error ? e.message : "sample pipeline failed";
  }
  setJob(job);
  return NextResponse.json({ id: job.id, status: job.status, error: job.error });
}
