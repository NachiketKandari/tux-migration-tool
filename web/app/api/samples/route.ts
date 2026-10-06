import { promises as fs } from "node:fs";
import { join } from "node:path";
import { NextResponse } from "next/server";
import { REPO_ROOT } from "@/lib/tuxconv";

// Curated one-click fixtures so the demo works without hunting for a .pc.
// This tree ships the tool, not sample sources: the list is empty until a
// deployment adds its own entries (each a path under a local fixtures dir).
const SAMPLES: { path: string; label: string; detail: string }[] = [];

export async function GET() {
  const out: { path: string; label: string; detail: string; bytes: number; available: boolean }[] = [];
  for (const s of SAMPLES) {
    try {
      const st = await fs.stat(join(REPO_ROOT, s.path));
      out.push({ ...s, bytes: st.size, available: st.isFile() });
    } catch {
      out.push({ ...s, bytes: 0, available: false });
    }
  }
  return NextResponse.json({ samples: out });
}
