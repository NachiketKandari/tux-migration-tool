import { promises as fs } from "node:fs";
import { join } from "node:path";
import { NextResponse } from "next/server";
import { getJob } from "@/lib/jobs";
import { buildLineage, parseLedgerSource, type LedgerMap } from "@/lib/lineage";

// GET /api/jobs/:id/lineage — evidence-based source→output trace.
//
// Two layers, strongest first (see web/lib/lineage.ts):
//   1. ledger — backend ground truth from conversion_logs/ledger/*.ledger.json
//      + the per-run audit copies (source lines → file via the plan/ledger
//      Map). Verified links, no token guessing.
//   2. heuristic — IR literal matching against file contents for everything
//      the ledger does not cover (Python/C#, scaffolding, unmapped arms).
//
// Pure derivation from artifacts already on disk — no CLI spawn, no LLM.

async function loadLedgerMaps(dir: string): Promise<LedgerMap[]> {
  const out: LedgerMap[] = [];
  const seen = new Set<string>();
  const candidates: string[] = [];

  // Live ledger dir (convertgo/convertcs persist here; cwd=job.dir at run time).
  try {
    for (const n of await fs.readdir(join(dir, "conversion_logs", "ledger"))) {
      if (/\.ledger\.json$/i.test(n)) candidates.push(join(dir, "conversion_logs", "ledger", n));
    }
  } catch {
    /* no ledger yet (Python target, pre-convert) */
  }
  // Per-run audit copies (<service>_ledger.json) — survive ledger rotation.
  try {
    for (const run of await fs.readdir(join(dir, "conversion_logs", "audit"))) {
      const runDir = join(dir, "conversion_logs", "audit", run);
      try {
        const st = await fs.stat(runDir);
        if (!st.isDirectory()) continue;
        for (const n of await fs.readdir(runDir)) {
          if (/_ledger\.json$/i.test(n)) candidates.push(join(runDir, n));
        }
      } catch {
        continue;
      }
    }
  } catch {
    /* no audit dir */
  }

  for (const p of candidates.slice(0, 8)) {
    try {
      const raw = await fs.readFile(p, "utf8");
      const doc = JSON.parse(raw) as { map?: { source?: string; target?: string }[] };
      for (const m of doc.map ?? []) {
        if (typeof m.source !== "string" || typeof m.target !== "string") continue;
        const key = `${m.source}→${m.target}`;
        if (seen.has(key)) continue;
        seen.add(key);
        const parsed = parseLedgerSource(m.source);
        if (!parsed) continue;
        out.push({ source: m.source, target: m.target, name: parsed.name, start: parsed.start, end: parsed.end });
        if (out.length >= 200) return out;
      }
    } catch {
      continue;
    }
  }
  return out;
}

export async function GET(_req: Request, { params }: { params: { id: string } }) {
  const job = getJob(params.id);
  if (!job) return NextResponse.json({ error: "unknown job" }, { status: 404 });
  if (!job.converted) return NextResponse.json({ error: "convert first — no converted tree yet" }, { status: 409 });

  const root = job.converted.root;
  const rel = job.converted.files ?? [];
  const files: { path: string; content: string }[] = [];
  for (const r of rel) {
    try {
      if (r.includes("..")) continue;
      const abs = join(root, r);
      if (!abs.startsWith(root)) continue;
      const st = await fs.stat(abs);
      if (!st.isFile()) continue;
      const content = await fs.readFile(abs, "utf8");
      files.push({ path: r, content });
    } catch {
      continue;
    }
  }

  const ledgerMaps = await loadLedgerMaps(job.dir);
  const result = buildLineage(job.ir, files, ledgerMaps);
  return NextResponse.json({ ...result, target: job.converted.target });
}
