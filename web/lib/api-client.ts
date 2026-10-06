// API client — single home for all browser → Next API calls.
// Keeps components thin: they call these helpers, never raw fetch URLs.
import type { JobState } from "@/hooks/use-job";

async function json<T>(res: Response): Promise<T> {
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error((data as { error?: string }).error ?? "request failed");
  return data as T;
}

export async function uploadFile(file: File): Promise<{ id: string }> {
  return uploadFiles([file]);
}

export async function uploadFiles(files: File[]): Promise<{ id: string }> {
  const fd = new FormData();
  for (const f of files) fd.append("files", f);
  const r = await fetch("/api/jobs", { method: "POST", body: fd });
  return json(r);
}

export async function runAnalysis(jobId: string): Promise<JobState> {
  const r = await fetch(`/api/jobs/${jobId}/analyze`, { method: "POST" });
  await json(r);
  return fetchJob(jobId);
}

export async function fetchJob(id: string): Promise<JobState> {
  const r = await fetch(`/api/jobs/${id}`);
  return json(r);
}

export async function discover(jobId: string, target: "go" | "cs", useLLM = false): Promise<JobState> {
  const r = await fetch(`/api/jobs/${jobId}/discover`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ target, useLLM }),
  });
  await json(r);
  return fetchJob(jobId);
}

export async function buildScenarios(jobId: string): Promise<JobState> {
  const r = await fetch(`/api/jobs/${jobId}/scenarios`, { method: "POST" });
  await json(r);
  return fetchJob(jobId);
}

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

export async function previewFilter(
  jobId: string,
  expr: string
): Promise<{ output: string; code: number; filter?: string; preview?: FilterPreview; previews?: FilterPreview[] }> {
  const r = await fetch(`/api/jobs/${jobId}/filter-preview`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ expr }),
  });
  return json(r);
}

export async function saveMapping(jobId: string, path: string, content: string): Promise<void> {
  const r = await fetch(`/api/jobs/${jobId}/mapping`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ path, content }),
  });
  await json(r);
}

export async function convert(
  jobId: string,
  target: "go" | "py" | "cs",
  useLLM = false
): Promise<{ job: JobState; note?: string; drafts?: unknown }> {
  const r = await fetch(`/api/jobs/${jobId}/convert`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ target, useLLM }),
  });
  const data = await json<{ note?: string; drafts?: unknown }>(r);
  const job = await fetchJob(jobId);
  return { job, note: data.note, drafts: data.drafts };
}

export interface GentestOptions {
  useLLM?: boolean;
  /** GT-7: --nice-names (literals-only LLM polish; needs useLLM). */
  niceNames?: boolean;
  /** GT-7: stage a complete -out snapshot and run the full test gate there. */
  staged?: boolean;
  /** GT-7: attach the job's runtime log (-log-file) for real fixture values. */
  withLog?: boolean;
}

export async function gentest(
  jobId: string,
  mode: "check" | "generate",
  opts: GentestOptions = {}
): Promise<JobState> {
  const r = await fetch(`/api/jobs/${jobId}/gentest`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ mode, ...opts }),
  });
  await json(r);
  return fetchJob(jobId);
}

/** GT-7: attach a runtime log (.txt/.log) to the job — tmpdir-only. */
export async function uploadGentestLog(jobId: string, file: File): Promise<JobState> {
  const fd = new FormData();
  fd.append("file", file);
  const r = await fetch(`/api/jobs/${jobId}/gentest/log`, { method: "POST", body: fd });
  await json(r);
  return fetchJob(jobId);
}

/** GT-7: detach the attached runtime log. */
export async function clearGentestLog(jobId: string): Promise<JobState> {
  const r = await fetch(`/api/jobs/${jobId}/gentest/log`, { method: "DELETE" });
  await json(r);
  return fetchJob(jobId);
}

export async function readConvertedFile(
  jobId: string,
  path: string,
  tree: "target" | "gentest" = "target"
): Promise<string> {
  const r = await fetch(
    `/api/jobs/${jobId}/files?path=${encodeURIComponent(path)}&tree=${tree}`
  );
  const d = await json<{ content: string }>(r);
  return d.content;
}

export async function saveConvertedFile(jobId: string, path: string, content: string): Promise<void> {
  const r = await fetch(`/api/jobs/${jobId}/files`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ path, content }),
  });
  await json(r);
}

export async function fetchMetrics(): Promise<{
  totals: { services: number; queries: number; files: number; tests: number; mappings: number; scenarios: number; edits: number };
  recent: { ts: string; kind: string; job: string; target?: string; queries?: number; files?: number; tests?: number; scenarios?: number; note?: string }[];
  count: number;
}> {
  const r = await fetch("/api/metrics");
  return json(r);
}

export async function fetchDbStatus(): Promise<{ enabled: boolean; driver: string; source: string; note: string }> {
  const r = await fetch("/api/db-status");
  return json(r);
}

export interface LLMStatus {
  anyKey: boolean;
  vllmKey: boolean;
  openrouterKey: boolean;
  profile: string;
  config: string;
  configPath?: string | null;
  note: string;
}

export async function fetchLLMStatus(): Promise<LLMStatus> {
  const r = await fetch("/api/llm-status");
  return json(r);
}

export function downloadArchiveUrl(jobId: string, tree?: "gentest"): string {
  return `/api/jobs/${jobId}/archive${tree ? `?tree=${tree}` : ""}`;
}

export async function loadSample(path: string): Promise<{ id: string }> {
  const r = await fetch("/api/samples/load", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ path }),
  });
  return json(r);
}

export async function listSamples(): Promise<
  { path: string; label: string; detail: string; bytes: number; available: boolean }[]
> {
  const r = await fetch("/api/samples");
  const d = await json<{ samples: { path: string; label: string; detail: string; bytes: number; available: boolean }[] }>(r);
  return d.samples;
}
