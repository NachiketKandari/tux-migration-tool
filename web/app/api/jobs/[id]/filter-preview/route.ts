import { promises as fs } from "node:fs";
import { join } from "node:path";
import { NextResponse } from "next/server";
import { getJob, setJob, makeLogger } from "@/lib/jobs";
import { runTux } from "@/lib/tuxconv";

export interface FilterPreview {
  entry: string;
  filter: string;
  mergedKey?: string;
  matched?: string[];
  pruned?: string[];
  blocks?: [number, number][];
  blockLines?: number;
  kept?: number;
  dropped?: number;
  unfolded?: number;
  reads?: string[];
  writes?: string[];
  queries?: string[];
  tx?: string[];
  residue?: string[];
  logicOnly?: boolean;
  flattened?: string;
  error?: string;
}

// POST /api/jobs/:id/filter-preview { expr } — read-only scenarioFilter fold
// preview. Runs the backend fold (`discover -filter <expr> -filter-json`)
// so the response carries the same ground truth as the console plus the
// flattened .pc the console never prints. The browser renders this
// verbatim — no expression parsing in the frontend.
export async function POST(req: Request, { params }: { params: { id: string } }) {
  const job = getJob(params.id);
  if (!job) return NextResponse.json({ error: "unknown job" }, { status: 404 });
  if (job.status === "running") return NextResponse.json({ error: "job is busy" }, { status: 409 });
  const body = (await req.json().catch(() => ({}))) as { expr?: string };
  const expr = (body.expr ?? "").trim();
  if (!expr) return NextResponse.json({ error: "expr is required" }, { status: 400 });
  if (expr.length > 500) return NextResponse.json({ error: "expr exceeds 500 chars" }, { status: 400 });

  job.status = "running";
  setJob(job);
  const log = makeLogger(job);
  const jsonPath = join(job.dir, "filter-preview.json");
  try {
    try {
      await fs.unlink(jsonPath);
    } catch {
      /* first run — nothing to clear */
    }
    const r = await runTux(job.dir, ["discover", job.sourcePath, "-filter", expr, "-filter-json", jsonPath], log);
    const output = (r.stdout || "") + (r.stderr ? `\n${r.stderr}` : "");

    let previews: FilterPreview[] = [];
    let filterEcho = expr;
    try {
      const raw = await fs.readFile(jsonPath, "utf8");
      const doc = JSON.parse(raw) as { filter?: string; previews?: FilterPreview[] };
      if (typeof doc.filter === "string") filterEcho = doc.filter;
      if (Array.isArray(doc.previews)) previews = doc.previews;
    } catch {
      /* no JSON — parse/flag error before the fold; fall through to text */
    }

    job.status = "done";
    setJob(job);
    // Single-entry jobs (the web norm) get a top-level preview for the
    // playground's code view; multi-file runs keep the full list.
    const preview = previews.length === 1 ? previews[0] : undefined;
    return NextResponse.json({
      status: "done",
      code: r.code,
      output: output.trim() || "(no output)",
      filter: filterEcho,
      preview,
      previews,
    });
  } catch (e) {
    job.status = "error";
    job.error = e instanceof Error ? e.message : "filter preview failed";
    setJob(job);
    return NextResponse.json({ status: "error", error: job.error }, { status: 500 });
  }
}
