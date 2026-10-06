import { NextResponse } from "next/server";
import { getJob } from "@/lib/jobs";

// GET /api/jobs/:id/logs?since=<n> — poll-based log tail (no SSE state).
export async function GET(req: Request, { params }: { params: { id: string } }) {
  const job = getJob(params.id);
  if (!job) return NextResponse.json({ error: "unknown job" }, { status: 404 });
  const since = Number(new URL(req.url).searchParams.get("since") ?? 0) || 0;
  return NextResponse.json({ status: job.status, lines: job.logs.slice(since), next: job.logs.length });
}
