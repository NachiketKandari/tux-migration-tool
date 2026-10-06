import { NextResponse } from "next/server";
import { readMetrics } from "@/lib/metrics";

// GET /api/metrics — cumulative master totals + recent runs.
// Derived from conversion_logs/web-metrics.jsonl; empty until the first run.
export async function GET() {
  const data = await readMetrics(50);
  return NextResponse.json(data);
}
