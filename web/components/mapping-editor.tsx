"use client";

import * as React from "react";
import { PencilLine, Loader2, Save, Copy, Check, RotateCcw, ArrowLeftRight } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Textarea } from "@/components/ui/textarea";
import { Skeleton } from "@/components/ui/skeleton";
import { RenameBar } from "@/components/files";
import { LLMToggle } from "@/components/llm-toggle";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";

export function MappingEditor({
  drafts,
  busy,
  onDraft,
  onSave,
  useLLM,
  onUseLLM,
  llmEffective,
  llmNote,
  noKey,
}: {
  drafts: { path: string; content: string }[];
  busy: boolean;
  onDraft: (target: "go" | "cs") => void;
  onSave: (path: string, content: string) => void;
  useLLM: boolean;
  onUseLLM: (v: boolean) => void;
  /** Whether the last draft actually carries AI-suggested names (backend ground truth). */
  llmEffective?: boolean | null;
  /** Backend note explaining the effective mode (fallback reason). */
  llmNote?: string | null;
  /** Server reports no LLM key — LLM-on will fall back. */
  noKey?: boolean;
}) {
  const [idx, setIdx] = React.useState(0);
  const [text, setText] = React.useState("");
  const [dirty, setDirty] = React.useState(false);
  const [copied, setCopied] = React.useState(false);
  const [showRename, setShowRename] = React.useState(false);

  React.useEffect(() => {
    setText(drafts[idx]?.content ?? "");
    setDirty(false);
    setShowRename(false);
  }, [drafts, idx]);

  const current = drafts[idx];
  const lines = React.useMemo(() => (text ? text.split("\n").length : 0), [text]);
  // Origin census — the answer to "why does it say deterministic?": count
  // the advisory-origin markers in the visible draft.
  const originCounts = React.useMemo(() => {
    if (!text) return { ai: 0, det: 0 };
    return {
      ai: (text.match(/ai-suggested/g) ?? []).length,
      det: (text.match(/# deterministic/g) ?? []).length,
    };
  }, [text]);

  function save() {
    if (current && !busy) {
      onSave(current.path, text);
      setDirty(false);
    }
  }

  async function copy() {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      /* clipboard unavailable */
    }
  }

  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="flex items-center gap-2 text-sm">
          <PencilLine className="h-4 w-4" />
          Step 1 — Mapping: draft, review, save
        </CardTitle>
        <CardDescription>
          Mapping comes first — conversion needs a reviewed mapping. Drafts are advisory defaults.
          Review names/routes, delete what you don&apos;t want, save, then convert.
          Press <kbd className="rounded border px-1 font-mono text-[10px]">⌘/Ctrl+S</kbd> to save.
          C# drafts are deterministic-only; the LLM toggle applies to Go drafts.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-2">
        <LLMToggle
          id="mapping-llm"
          value={useLLM}
          onChange={onUseLLM}
          disabled={busy}
          hint="Go naming only — off works with no key; on needs VLLM_API_KEY / OPENROUTER_API_KEY in the server env"
          noKey={noKey}
        />
        {llmNote && drafts.length > 0 && (
          <p
            className={
              llmEffective
                ? "rounded-md border border-green-500/30 bg-green-500/10 p-2 text-xs"
                : "rounded-md border border-amber-500/30 bg-amber-500/10 p-2 text-xs"
            }
            role="status"
          >
            {useLLM ? "LLM requested" : "LLM off"} · effective: {llmEffective ? "AI" : "deterministic"} — {llmNote}
          </p>
        )}
        <div className="flex flex-wrap items-center gap-2">
          <Button size="sm" variant="secondary" disabled={busy} onClick={() => onDraft("go")}>
            {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : null}
            Draft Go mapping
          </Button>
          <Button size="sm" variant="secondary" disabled={busy} onClick={() => onDraft("cs")}>
            Draft C# mapping
          </Button>
          {drafts.length > 1 && (
            <Select value={String(idx)} onValueChange={(v) => setIdx(Number(v))}>
              <SelectTrigger className="h-8 w-[220px] text-xs" aria-label="Select draft">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {drafts.map((d, i) => (
                  <SelectItem key={d.path} value={String(i)}>
                    {d.path.split("/").pop()}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          )}
          {current && dirty && <Badge variant="outline">edited</Badge>}
          {current && (
            <span className="text-[11px] tabular-nums text-muted-foreground">
              {lines} lines · {text.length} chars
              {originCounts.ai + originCounts.det > 0 &&
                ` · ${originCounts.ai} ai-suggested · ${originCounts.det} deterministic`}
            </span>
          )}
          <span className="ml-auto flex gap-1.5">
            {current && (
              <>
                <Button size="sm" variant="ghost" disabled={busy || !text} onClick={copy} title="Copy mapping">
                  {copied ? <Check className="h-3.5 w-3.5" /> : <Copy className="h-3.5 w-3.5" />}
                  {copied ? "Copied" : "Copy"}
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  disabled={busy || !dirty}
                  onClick={() => {
                    setText(current.content);
                    setDirty(false);
                  }}
                  title="Discard edits"
                >
                  <RotateCcw className="h-3.5 w-3.5" />
                  Discard
                </Button>
                <Button size="sm" variant="ghost" onClick={() => setShowRename((s) => !s)} aria-expanded={showRename}>
                  <ArrowLeftRight className="h-3.5 w-3.5" />
                  Rename
                </Button>
              </>
            )}
            <Button size="sm" disabled={busy || !current} onClick={save}>
              {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Save className="h-3.5 w-3.5" />}
              Save &amp; continue
            </Button>
          </span>
        </div>
        {busy && drafts.length === 0 ? (
          <MappingSkeleton />
        ) : drafts.length === 0 ? (
          <p className="rounded-md border border-dashed p-4 text-center text-xs text-muted-foreground">
            No drafts yet — run one of the draft passes above.
          </p>
        ) : (
          <>
            {showRename && (
              <RenameBar
                content={text}
                onApply={(next) => {
                  setText(next);
                  setDirty(true);
                }}
              />
            )}
            <Textarea
              rows={22}
              value={text}
              onChange={(e) => {
                setText(e.target.value);
                setDirty(true);
              }}
              onKeyDown={(e) => {
                if ((e.metaKey || e.ctrlKey) && e.key === "s") {
                  e.preventDefault();
                  save();
                }
              }}
              spellCheck={false}
              aria-label="Mapping YAML"
            />
          </>
        )}
      </CardContent>
    </Card>
  );
}

export function MappingSkeleton() {
  return (
    <div className="space-y-2" aria-label="Loading mapping">
      <div className="flex gap-2">
        <Skeleton className="h-8 w-32" />
        <Skeleton className="h-8 w-32" />
        <Skeleton className="ml-auto h-8 w-36" />
      </div>
      <Skeleton className="h-[420px] w-full" />
    </div>
  );
}
