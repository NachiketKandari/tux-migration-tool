// Master metrics log — one append-only JSONL file every web run contributes to.
//
// File: <repo-root>/conversion_logs/web-metrics.jsonl (gitignored, persistent
// across disposable job tmpdirs). Each line is one event; totals are derived
// by scanning the file, so future metrics only need a new numeric field here
// plus one recordMetric call at the producer site.
//
// Kept deliberately small: 6 cumulative counters + recent-run table. No DB.
import { promises as fs } from "node:fs";
import { join } from "node:path";
import { REPO_ROOT } from "@/lib/tuxconv";

export const METRICS_FILE = join(REPO_ROOT, "conversion_logs", "web-metrics.jsonl");

export type MetricKind = "parse" | "discover" | "scenarios" | "mapping" | "convert" | "gentest" | "edit";

export interface MetricEvent {
  ts: string;
  kind: MetricKind;
  job: string;
  target?: string;
  /** Query units found (parse) or converted (convert). */
  queries?: number;
  /** Converted files written (convert) or test files (gentest). */
  files?: number;
  /** Generated test files (gentest generate only). */
  tests?: number;
  /** Scenario slices built (scenarios). */
  scenarios?: number;
  /** GT-7: gentest methods whose fixtures came from a runtime log. */
  fixturesFromLog?: number;
  /** GT-7: gentest methods that fell back to assumed placeholders. */
  fixturesAssumed?: number;
  /** GT-7: whether the gentest run requested --nice-names. */
  niceNames?: boolean;
  /** GT-7: whether the gentest run staged a -out snapshot. */
  staged?: boolean;
  note?: string;
}

export interface MetricTotals {
  services: number;
  queries: number;
  files: number;
  tests: number;
  mappings: number;
  scenarios: number;
  edits: number;
}

// Best-effort append: metrics must never fail a user run.
export async function recordMetric(e: Omit<MetricEvent, "ts">): Promise<void> {
  try {
    await fs.mkdir(join(REPO_ROOT, "conversion_logs"), { recursive: true });
    const line = JSON.stringify({ ...e, ts: new Date().toISOString() }) + "\n";
    await fs.appendFile(METRICS_FILE, line, "utf8");
  } catch {
    /* metrics are advisory */
  }
}

export async function readMetrics(limit = 50): Promise<{ totals: MetricTotals; recent: MetricEvent[]; count: number }> {
  const totals: MetricTotals = { services: 0, queries: 0, files: 0, tests: 0, mappings: 0, scenarios: 0, edits: 0 };
  let lines: string[] = [];
  try {
    const raw = await fs.readFile(METRICS_FILE, "utf8");
    lines = raw.split("\n").filter(Boolean);
  } catch {
    return { totals, recent: [], count: 0 };
  }
  const events: MetricEvent[] = [];
  for (const ln of lines) {
    try {
      events.push(JSON.parse(ln) as MetricEvent);
    } catch {
      continue;
    }
  }
  for (const e of events) {
    if (e.kind === "convert") {
      totals.services += 1;
      totals.queries += e.queries ?? 0;
      totals.files += e.files ?? 0;
    } else if (e.kind === "gentest") {
      totals.tests += e.tests ?? 0;
    } else if (e.kind === "mapping") {
      totals.mappings += 1;
    } else if (e.kind === "scenarios") {
      totals.scenarios += e.scenarios ?? 0;
    } else if (e.kind === "edit") {
      totals.edits += 1;
    }
    // parse/discover feed the recent table only — converted counts live on convert.
  }
  return { totals, recent: events.slice(-limit).reverse(), count: events.length };
}

// Server-side query-unit counter (mirrors web/lib/ir asArray for the known keys).
export function countQueries(ir: unknown): number {
  if (!ir || typeof ir !== "object") return 0;
  const o = ir as Record<string, unknown>;
  for (const k of ["queries", "Queries", "queryUnits", "QueryUnits"]) {
    if (Array.isArray(o[k])) return (o[k] as unknown[]).length;
  }
  const lower = new Map(Object.keys(o).map((k) => [k.toLowerCase(), k]));
  for (const k of ["queries", "queryunits"]) {
    const hit = lower.get(k);
    if (hit && Array.isArray(o[hit])) return (o[hit] as unknown[]).length;
  }
  return 0;
}
