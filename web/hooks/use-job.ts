"use client";

import * as React from "react";
import type {
  FlowReport,
  ScenarioBundle,
  AnalysisRow,
  GentestLogInfo,
  GentestPreflight,
  GentestSummary,
} from "@/lib/jobs";

export interface JobState {
  id: string;
  name: string;
  status: "ready" | "running" | "done" | "error";
  error?: string;
  inputFiles?: string[];
  isBatch?: boolean;
  ir?: Record<string, unknown>;
  irList?: { name: string; ir: Record<string, unknown> }[];
  flowText?: string;
  flowReport?: FlowReport;
  sourcePreview?: string;
  sourcePreviews?: { name: string; preview: string }[];
  analysis?: AnalysisRow[];
  analysisCsv?: string;
  scenarios?: ScenarioBundle;
  drafts?: { path: string; content: string }[];
  mappingPath?: string;
  mappingLLM?: boolean;
  mappingLLMEffective?: boolean;
  mappingLLMNote?: string;
  converted?: { target: string; root: string; files: string[]; summary: string; llm?: boolean; llmEffective?: boolean; llmNote?: string; llmCalls?: number };
  gentestGap?: string;
  gentestFiles?: string[];
  gentestLog?: GentestLogInfo;
  gentestPreflight?: GentestPreflight;
  gentestSummary?: GentestSummary;
  gentestNiceNames?: boolean;
  gentestLLM?: boolean;
  gentestLLMEffective?: boolean;
  gentestLLMNote?: string;
  gentestOutRoot?: string;
}

export function useJob(jobId: string | null) {
  const [job, setJob] = React.useState<JobState | null>(null);
  const [logs, setLogs] = React.useState<string[]>([]);
  const logsRef = React.useRef(0);

  const refresh = React.useCallback(async (id: string) => {
    const r = await fetch(`/api/jobs/${id}`);
    if (r.ok) setJob(await r.json());
  }, []);

  // Poll job state while busy.
  React.useEffect(() => {
    if (!jobId) return;
    refresh(jobId);
    const t = setInterval(() => refresh(jobId), 1500);
    return () => clearInterval(t);
  }, [jobId, refresh]);

  // Poll logs.
  React.useEffect(() => {
    if (!jobId) return;
    const t = setInterval(async () => {
      const r = await fetch(`/api/jobs/${jobId}/logs?since=${logsRef.current}`);
      if (r.ok) {
        const d = await r.json();
        if (d.lines?.length) setLogs((prev) => [...prev, ...d.lines]);
        logsRef.current = d.next;
      }
    }, 1500);
    return () => clearInterval(t);
  }, [jobId]);

  const resetLogs = React.useCallback(() => {
    setLogs([]);
    logsRef.current = 0;
  }, []);

  return { job, setJob, logs, resetLogs, refresh };
}
