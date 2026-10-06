import { NextResponse } from "next/server";
import { getLLMStatus } from "@/lib/llm-status";

// GET /api/llm-status — do LLM-on runs reach a model from this server?
// Reports key presence only (never values): env vars plus the resolved yaml
// literals (models[].apiKey), env-first — the same resolution the CLI
// uses. The UI uses this to warn upfront instead of looking broken
// after a run.
export async function GET() {
  return NextResponse.json(getLLMStatus());
}
