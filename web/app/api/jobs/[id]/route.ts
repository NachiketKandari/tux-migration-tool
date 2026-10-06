import { NextResponse } from "next/server";
import { getJob } from "@/lib/jobs";

function pub(id: string) {
  const job = getJob(id);
  if (!job) return null;
  const { dir: _d, sourcePath: _s, ...rest } = job;
  void _d;
  void _s;
  return rest;
}

export async function GET(_req: Request, { params }: { params: { id: string } }) {
  const j = pub(params.id);
  if (!j) return NextResponse.json({ error: "unknown job" }, { status: 404 });
  return NextResponse.json(j);
}
