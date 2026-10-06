import { promises as fs } from "node:fs";
import { join } from "node:path";
import { NextResponse } from "next/server";
import { getJob, setJob } from "@/lib/jobs";

// POST   /api/jobs/:id/gentest/log — attach the target app's runtime log.
// DELETE /api/jobs/:id/gentest/log — detach it.
//
// GT-7: the log feeds `gentest -log-file` with real fixture values. It is
// stored inside the job tmpdir only (<job>/gentest/runtime.log) and is never
// bundled into the Download .zip or echoed into metrics — runtime logs can
// carry request bodies and internal endpoints. Content is size-capped and
// written as UTF-8; the CLI parser tolerates malformed lines with warnings.
const MAX_LOG = 5 * 1024 * 1024;

function logPath(jobDir: string): string {
  return join(jobDir, "gentest", "runtime.log");
}

export async function POST(req: Request, { params }: { params: { id: string } }) {
  const job = getJob(params.id);
  if (!job) return NextResponse.json({ error: "unknown job" }, { status: 404 });
  if (job.status === "running") return NextResponse.json({ error: "job is busy" }, { status: 409 });

  const ctype = req.headers.get("content-type") ?? "";
  let name = "runtime.log";
  let text = "";
  if (ctype.includes("multipart/form-data")) {
    const form = await req.formData();
    const f = form.get("file");
    if (!(f instanceof File)) {
      return NextResponse.json({ error: "field 'file' is required (.txt/.log)" }, { status: 400 });
    }
    if (f.size > MAX_LOG) {
      return NextResponse.json({ error: `log exceeds the 5MB limit (${f.size} bytes)` }, { status: 400 });
    }
    name = (f.name || "runtime.log").replace(/[^\w.\-+@]+/g, "_");
    text = await f.text();
  } else {
    const body = (await req.json().catch(() => ({}))) as { name?: string; text?: string };
    if (typeof body.text !== "string" || body.text.trim() === "") {
      return NextResponse.json({ error: "provide a .txt/.log file or pasted log text" }, { status: 400 });
    }
    if (body.name) name = String(body.name).replace(/[^\w.\-+@]+/g, "_") || "runtime.log";
    text = body.text;
  }
  const bytes = Buffer.byteLength(text, "utf8");
  if (bytes === 0) return NextResponse.json({ error: "log is empty" }, { status: 400 });
  if (bytes > MAX_LOG) return NextResponse.json({ error: "log exceeds the 5MB limit" }, { status: 400 });

  await fs.mkdir(join(job.dir, "gentest"), { recursive: true });
  await fs.writeFile(logPath(job.dir), text, "utf8");
  job.gentestLog = { name, bytes, savedAt: new Date().toISOString() };
  // A replaced log invalidates the old preflight and generate summary.
  job.gentestPreflight = undefined;
  job.gentestSummary = undefined;
  setJob(job);
  return NextResponse.json({ log: job.gentestLog });
}

export async function DELETE(_req: Request, { params }: { params: { id: string } }) {
  const job = getJob(params.id);
  if (!job) return NextResponse.json({ error: "unknown job" }, { status: 404 });
  if (job.status === "running") return NextResponse.json({ error: "job is busy" }, { status: 409 });
  await fs.rm(logPath(job.dir), { force: true });
  job.gentestLog = undefined;
  job.gentestPreflight = undefined;
  job.gentestSummary = undefined;
  setJob(job);
  return NextResponse.json({ log: null });
}
