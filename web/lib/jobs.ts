// Job store: in-memory cache backed by per-job tmpdir snapshots (job.json).
// Jobs live in disposable tmpdirs; no database.
//
// Why disk-backed: Next dev compiles route bundles independently and
// reloads drop module state, so this Map is only a cache — the tmpdir
// snapshot is the source of truth. All reads go through getJob, which
// rehydrates from disk on a cache miss (sync fs: job counts are tiny).
export type JobStatus = "ready" | "running" | "done" | "error";

export interface DraftFile {
  path: string;
  content: string;
}

export interface ConvertedTree {
  target: string;
  root: string;
  files: string[];
  summary: string;
  /** Whether the run was requested with the LLM seam (false = deterministic -no-llm). */
  llm?: boolean;
  /** Whether the run actually made LLM calls (false = deterministic fallback). */
  llmEffective?: boolean;
  /** Human-readable note explaining the effective mode (fallback reason, call count). */
  llmNote?: string;
  /** LLM calls reported on the CLI stdout tail ("N llm calls"), when parsed. */
  llmCalls?: number;
}

export interface FlowCoverage {
  classified: number;
  codeLines: number;
  unknown: number;
  residue?: number[];
}

export interface FlowHint {
  kind: string;
  line: number;
  detail: string;
}

export interface FlowFunction {
  name: string;
  startLine?: number;
  endLine?: number;
  coverage?: FlowCoverage;
  hints?: FlowHint[];
  /** Raw tree node kept for the visual explorer (opaque, versioned by CLI). */
  tree?: unknown;
}

export interface FlowReport {
  target: string;
  files: { path: string; functions: FlowFunction[] }[];
}

export interface ScenarioArtifact {
  /** File name relative to the scenarios/ dir, e.g. SVC_X.trn_cd_A.pc */
  name: string;
  /** Rendered flattened source — the whole file, uncapped, held in the job bundle. */
  content: string;
}

export interface ScenarioBundle {
  axesText: string;
  axesMd?: string;
  sharedMd?: string;
  sharedJson?: unknown;
  artifacts: ScenarioArtifact[];
  note?: string;
}

export interface AnalysisRow {
  file: string;
  numLines: number;
  numQueries: number;
  branchingFactor: number;
  branchCount: number;
  hasTpcall: boolean;
  tpcallCount: number;
  fnLocalCount: number;
  fnExternalCount: number;
  complexityScore: number;
  complexity: string;
  reasons: string;
}

/** GT-7: one attached runtime log (job-tmpdir only, never leaves the machine). */
export interface GentestLogInfo {
  name: string;
  bytes: number;
  savedAt: string;
}

/** GT-7: log-fixture coverage from the check-only preflight. */
export interface GentestPreflight {
  path: string;
  lines: number;
  traces: number;
  complete: number;
  warnings: number;
  fromLog: number;
  assumed: number;
}

/** GT-7: one generated function's fixture provenance. */
export interface GentestFixture {
  service: string;
  layer: string;
  func: string;
  /** "log <short-id>" when a complete trace supplied values, "assumed" otherwise. */
  source: string;
}

/** GT-7: one file written/staged by a generate run, rooted in the target
 * tree ("target") or the -out snapshot ("gentest"). */
export interface GentestFileEntry {
  path: string;
  tree: "target" | "gentest";
}

/** GT-7: the parsed gentest_summary.json of the newest generate run. */
export interface GentestSummary {
  fixtures: GentestFixture[];
  gates: string[];
  testsFailed: boolean;
  llmCalls: number;
  files: GentestFileEntry[];
  staged: string[];
  fromLog: number;
  assumed: number;
}

export interface Job {
  id: string;
  name: string;
  dir: string;
  sourcePath: string;
  /** All uploaded source basenames (batch jobs). Single-file jobs carry one. */
  inputFiles?: string[];
  /** True when the job holds more than one source file. */
  isBatch?: boolean;
  status: JobStatus;
  error?: string;
  ir?: Record<string, unknown>;
  /** Per-file IR for batch jobs (name + parsed IR). */
  irList?: { name: string; ir: Record<string, unknown> }[];
  flowText?: string;
  flowReport?: FlowReport;
  sourcePreview?: string;
  /** Per-file source previews for batch jobs (capped). */
  sourcePreviews?: { name: string; preview: string }[];
  analysis?: AnalysisRow[];
  analysisCsv?: string;
  scenarios?: ScenarioBundle;
  drafts?: DraftFile[];
  mappingPath?: string;
  /** Whether the mapping draft was requested with the LLM naming seam. */
  mappingLLM?: boolean;
  /** Whether the draft actually carries AI-suggested names (false = deterministic fallback). */
  mappingLLMEffective?: boolean;
  /** Human-readable note explaining the effective mapping mode. */
  mappingLLMNote?: string;
  converted?: ConvertedTree;
  gentestGap?: string;
  gentestFiles?: string[];
  /** GT-7: attached runtime log (job tmpdir only). */
  gentestLog?: GentestLogInfo;
  /** GT-7: log-fixture coverage from the last check-only preflight. */
  gentestPreflight?: GentestPreflight;
  /** GT-7: structured outcome of the last generate run (audit summary). */
  gentestSummary?: GentestSummary;
  /** GT-7: whether the run requested --nice-names. */
  gentestNiceNames?: boolean;
  /** GT-7: whether the run requested the LLM seam (false = -no-llm). */
  gentestLLM?: boolean;
  /** GT-7: whether the run actually made LLM calls. */
  gentestLLMEffective?: boolean;
  /** GT-7: human-readable note explaining the effective mode. */
  gentestLLMNote?: string;
  /** GT-7: -out snapshot root when the last run staged one. */
  gentestOutRoot?: string;
  logs: string[];
}

import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const jobs = new Map<string, Job>();

function snapshotPath(dir: string): string {
  return join(dir, "job.json");
}

export function getJob(id: string): Job | undefined {
  const hit = jobs.get(id);
  if (hit) return hit;
  // Cache miss (dev reload, fresh worker): scan tmpdir snapshots.
  let entries: string[] = [];
  try {
    entries = readdirSync(tmpdir());
  } catch {
    return undefined;
  }
  for (const e of entries) {
    if (!e.startsWith("tux-web-")) continue;
    try {
      const snap = JSON.parse(readFileSync(join(tmpdir(), e, "job.json"), "utf8")) as Job;
      if (snap && snap.id === id) {
        jobs.set(id, snap);
        return snap;
      }
    } catch {
      continue;
    }
  }
  return undefined;
}

export function setJob(job: Job): void {
  jobs.set(job.id, job);
  persist(job);
}

function persist(job: Job): void {
  try {
    writeFileSync(
      snapshotPath(job.dir),
      JSON.stringify({ ...job, logs: job.logs.slice(-500) })
    );
  } catch {
    /* tmpdir gone — nothing to persist */
  }
}

// makeLogger appends to the job log and re-persists at most every 2s, so
// polling routes see a live tail even across bundles/reloads mid-run.
export function makeLogger(job: Job): (line: string) => void {
  let last = 0;
  return (line: string) => {
    job.logs.push(line);
    const now = Date.now();
    if (now - last > 2000) {
      last = now;
      persist(job);
    }
  };
}
