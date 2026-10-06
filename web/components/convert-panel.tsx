"use client";

import * as React from "react";
import { Hammer, Loader2, Braces, FileCode2, Container, Waypoints, Download } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { ScrollArea } from "@/components/ui/scroll-area";
import { FileTree, CodeView, FileTreeSkeleton, CodeViewSkeleton } from "@/components/files";
import { LLMToggle } from "@/components/llm-toggle";
import { CONVERT_TARGETS, type ConvertTarget } from "@/lib/targets";
import { downloadArchiveUrl } from "@/lib/api-client";
import { cn } from "@/lib/utils";

const ICONS: Record<ConvertTarget, React.ReactNode> = {
  go: <Braces className="h-4 w-4" />,
  py: <FileCode2 className="h-4 w-4" />,
  cs: <Container className="h-4 w-4" />,
};

export function ConvertPanel({
  target,
  onTarget,
  onConvert,
  busy,
  hasMapping,
  useLLM,
  onUseLLM,
  onGoMapping,
  converted,
  onOpenFile,
  onSaveFile,
  savingFile,
  selFile,
  fileContent,
  loadingFile,
  provenance,
  onGoTrace,
  jobId,
  noKey,
}: {
  target: ConvertTarget;
  onTarget: (t: ConvertTarget) => void;
  onConvert: () => void;
  busy: boolean;
  hasMapping: boolean;
  useLLM: boolean;
  onUseLLM: (v: boolean) => void;
  onGoMapping?: () => void;
  converted?: {
    target: string;
    root: string;
    files: string[];
    summary: string;
    llm?: boolean;
    llmEffective?: boolean;
    llmNote?: string;
    llmCalls?: number;
  };
  onOpenFile: (p: string) => void;
  onSaveFile: (path: string, content: string) => void;
  savingFile: boolean;
  selFile: string | null;
  fileContent: string;
  loadingFile: boolean;
  provenance?: { node: string; kind: string; reason: string; confidence: string; verified?: boolean }[];
  onGoTrace?: () => void;
  /** Job id — enables the Download-all-.zip button. */
  jobId?: string | null;
  /** Server reports no LLM key — LLM-on will fall back. */
  noKey?: boolean;
}) {
  const modeBadge = !converted ? null : converted.llmEffective ? (
    <Badge variant="secondary">
      {converted.files.length} files · llm
      {converted.llmCalls != null && converted.llmCalls > 0 ? ` (${converted.llmCalls} calls)` : ""}
    </Badge>
  ) : converted.llm ? (
    <Badge variant="outline" title={converted.llmNote ?? "LLM requested but the run fell back to deterministic output"}>
      {converted.files.length} files · deterministic fallback
    </Badge>
  ) : (
    <Badge variant="secondary">{converted.files.length} files · deterministic</Badge>
  );
  return (
    <div className="space-y-3">
      <div className="grid grid-cols-1 gap-3 md:grid-cols-3" role="radiogroup" aria-label="Conversion target">
        {CONVERT_TARGETS.map((t) => {
          const active = target === t.id;
          return (
            <button
              key={t.id}
              role="radio"
              aria-checked={active}
              onClick={() => onTarget(t.id)}
              className={cn(
                "rounded-xl border p-4 text-left transition-colors",
                active ? "border-primary bg-primary/5 shadow-sm" : "hover:border-primary/50 hover:bg-accent/50"
              )}
            >
              <div className="flex items-center gap-2">
                <span className={cn("flex h-8 w-8 items-center justify-center rounded-lg", active ? "bg-primary text-primary-foreground" : "bg-muted text-muted-foreground")}>
                  {ICONS[t.id]}
                </span>
                <span className="text-sm font-bold">{t.title}</span>
                {active && <Badge className="ml-auto">selected</Badge>}
              </div>
              <p className="mt-2 text-xs text-muted-foreground">{t.blurb}</p>
              <p className="mt-1 font-mono text-[11px] text-muted-foreground">
                <span className="text-foreground">{t.command}</span> → {t.output}
              </p>
            </button>
          );
        })}
      </div>

      <Card>
        <CardHeader className="pb-2">
          <div className="flex flex-wrap items-center gap-2">
            <CardTitle className="flex items-center gap-2 text-sm">
              <Hammer className="h-4 w-4" />
              Step 2 — Convert: mapping first, then code
            </CardTitle>
            {busy ? (
              <Badge variant="secondary" className="flex items-center gap-1">
                <Loader2 className="h-3 w-3 animate-spin" /> converting…
              </Badge>
            ) : (
              modeBadge
            )}
            {!hasMapping && (target === "go" || target === "cs") && (
              <Badge variant="outline">Step 1 required — draft & save a mapping first</Badge>
            )}
            {converted && jobId && (
              <Button size="sm" variant="outline" asChild title="Download the whole converted tree + mapping as a .zip">
                <a href={downloadArchiveUrl(jobId)}>
                  <Download className="h-3.5 w-3.5" />
                  Download .zip
                </a>
              </Button>
            )}
            <Button
              size="sm"
              className="ml-auto"
              disabled={busy || (!hasMapping && (target === "go" || target === "cs"))}
              onClick={onConvert}
              title={
                !hasMapping && (target === "go" || target === "cs")
                  ? "Complete Step 1 (mapping) first"
                  : `Convert to ${target}`
              }
            >
              {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Hammer className="h-3.5 w-3.5" />}
              Convert to {target === "go" ? "Go" : target === "py" ? "Python" : "C#"}
            </Button>
          </div>
          <CardDescription>
            Mapping-first: Go/C# need a reviewed mapping from Step 1 (Python converts directly).
            LLM off runs <code className="font-mono">-no-llm</code> — deterministic drafts with SQL-fidelity
            gates, no key needed. LLM on uses the seam when a key resolves, otherwise falls back
            deterministically. Open a file to edit it in place; renames apply file-wide.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-2">
          <LLMToggle
            id="convert-llm"
            value={useLLM}
            onChange={onUseLLM}
            disabled={busy}
            hint="off works with no key; on needs VLLM_API_KEY / OPENROUTER_API_KEY in the server env"
            noKey={noKey}
          />
          {converted?.llmNote && (
            <p
              className={
                converted.llmEffective
                  ? "rounded-md border border-green-500/30 bg-green-500/10 p-2 text-xs"
                  : converted.llm
                    ? "rounded-md border border-amber-500/30 bg-amber-500/10 p-2 text-xs"
                    : "rounded-md border p-2 text-xs text-muted-foreground"
              }
              role="status"
            >
              {converted.llm ? "LLM requested" : "LLM off"} · effective:{" "}
              {converted.llmEffective ? "AI" : "deterministic"} — {converted.llmNote}
            </p>
          )}
          {!hasMapping && (target === "go" || target === "cs") && onGoMapping && (
            <div className="flex flex-wrap items-center gap-2 rounded-md border border-dashed p-3 text-xs text-muted-foreground">
              <span>No mapping yet — Step 1 drafts it. Save the draft, then convert.</span>
              <Button size="sm" variant="outline" className="ml-auto" onClick={onGoMapping}>
                Go to Mapping (Step 1)
              </Button>
            </div>
          )}
          {converted && <pre className="mb-3 whitespace-pre-wrap font-mono text-xs text-muted-foreground">{converted.summary}</pre>}
          {converted && (
            <p className="rounded-md border p-2 text-[11px] text-muted-foreground">
              Stored on the server under <code className="font-mono">{converted.root}</code> (disposable job
              tmpdir — vanishes on restart). Use <span className="font-medium">Download .zip</span> above for
              the durable copy (converted tree + mapping + summary), or open a file for per-file Download.
            </p>
          )}
          <div className="grid grid-cols-1 gap-3 md:grid-cols-[280px_minmax(0,1fr)]">
            <ScrollArea className="max-h-[480px]">
              {busy && !converted ? (
                <FileTreeSkeleton />
              ) : (
                <FileTree files={converted?.files ?? []} selected={selFile} onSelect={onOpenFile} />
              )}
            </ScrollArea>
            <div className="min-w-0">
              {selFile && (provenance?.length ?? 0) > 0 && (
                <div className="trace-item mb-2 flex flex-wrap items-center gap-1.5 rounded-lg border border-primary/25 bg-primary/5 px-2.5 py-2" aria-live="polite">
                  <Waypoints className="h-3.5 w-3.5 shrink-0 text-primary" />
                  <span className="text-[11px] font-medium">This file exists because of:</span>
                  {provenance!.slice(0, 4).map((p) => (
                    <Badge
                      key={p.node}
                      variant={p.verified || p.confidence === "exact" ? "default" : "secondary"}
                      className="max-w-[220px] truncate font-mono text-[10px]"
                      title={`${p.node} — ${p.reason}${p.verified ? " (ledger-verified)" : ""}`}
                    >
                      {p.verified ? "✓ " : ""}
                      {p.node}
                    </Badge>
                  ))}
                  {provenance!.length > 4 && (
                    <span className="text-[11px] tabular-nums text-muted-foreground">+{provenance!.length - 4} more</span>
                  )}
                  {onGoTrace && (
                    <Button size="sm" variant="ghost" className="ml-auto h-6 px-2 text-[11px]" onClick={onGoTrace}>
                      Full trace <Waypoints className="h-3 w-3" />
                    </Button>
                  )}
                </div>
              )}
              <ScrollArea className="max-h-[480px]">
                {loadingFile ? (
                  <CodeViewSkeleton />
                ) : selFile ? (
                  <CodeView
                    content={fileContent}
                    path={selFile}
                    editable
                    saving={savingFile}
                    onSave={(next) => onSaveFile(selFile, next)}
                  />
                ) : (
                  <p className="p-3 text-xs text-muted-foreground">Select a file to view it.</p>
                )}
              </ScrollArea>
            </div>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}
