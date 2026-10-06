import { promises as fs } from "node:fs";
import { relative } from "node:path";
import { NextResponse } from "next/server";
import { getJob, setJob } from "@/lib/jobs";
import { recordMetric } from "@/lib/metrics";

function inside(jobDir: string, p: string): boolean {
  const rel = relative(jobDir, p);
  return rel !== "" && !rel.startsWith("..");
}

// PUT /api/jobs/:id/mapping { path, content } — save an edited draft yaml.
export async function PUT(req: Request, { params }: { params: { id: string } }) {
  const job = getJob(params.id);
  if (!job) return NextResponse.json({ error: "unknown job" }, { status: 404 });
  const body = (await req.json().catch(() => ({}))) as { path?: string; content?: string };
  if (!body.path || typeof body.content !== "string") {
    return NextResponse.json({ error: "path and content are required" }, { status: 400 });
  }
  if (!inside(job.dir, body.path)) {
    return NextResponse.json({ error: "path escapes the job directory" }, { status: 400 });
  }
  if (body.content.length > 512 * 1024) {
    return NextResponse.json({ error: "mapping exceeds 512KB" }, { status: 400 });
  }
  await fs.writeFile(body.path, body.content);
  job.mappingPath = body.path;
  setJob(job);
  await recordMetric({ kind: "mapping", job: job.name, note: body.path.split("/").pop() });
  const { dir: _d, sourcePath: _s, ...rest } = job;
  void _d;
  void _s;
  return NextResponse.json({ ok: true, mappingPath: job.mappingPath, job: rest });
}
