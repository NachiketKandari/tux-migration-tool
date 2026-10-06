"use client";

import * as React from "react";
import {
  AlertTriangle,
  CheckCircle2,
  Download,
  FileText,
  FlaskConical,
  Loader2,
  Play,
  Upload,
  X,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Skeleton } from "@/components/ui/skeleton";
import { Textarea } from "@/components/ui/textarea";
import { LLMToggle } from "@/components/llm-toggle";
import { CodeView } from "@/components/files";
import { readConvertedFile, downloadArchiveUrl } from "@/lib/api-client";
import type { GentestFixture, GentestLogInfo, GentestPreflight, GentestSummary } from "@/lib/jobs";
import type { GentestOptions } from "@/lib/api-client";

export interface TestRunOptions extends GentestOptions {
  useLLM: boolean;
  niceNames: boolean;
  staged: boolean;
  withLog: boolean;
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

function SourceBadge({ source }: { source: string }) {
  const assumed = source === "assumed";
  return (
    <Badge variant={assumed ? "outline" : "secondary"} className={assumed ? "border-amber-500/60 text-amber-600" : ""}>
      {source}
    </Badge>
  );
}

export function TestsPanel({
  jobId,
  busy,
  canRun,
  gap,
  testFiles,
  log,
  preflight,
  summary,
  hasSnapshot,
  llmKeyOk,
  onGap,
  onGenerate,
  onGoConvert,
  onUploadLog,
  onClearLog,
}: {
  jobId: string | null;
  busy: boolean;
  canRun: boolean;
  gap?: string;
  testFiles?: string[];
  log?: GentestLogInfo;
  preflight?: GentestPreflight;
  summary?: GentestSummary;
  hasSnapshot?: boolean;
  llmKeyOk?: boolean;
  onGap: (opts: TestRunOptions) => void;
  onGenerate: (opts: TestRunOptions) => void;
  onGoConvert?: () => void;
  onUploadLog: (file: File) => void;
  onClearLog: () => void;
}) {
  const [useLLM, setUseLLM] = React.useState(false);
  const [niceNames, setNiceNames] = React.useState(false);
  const [staged, setStaged] = React.useState(false);
  const [withLog, setWithLog] = React.useState(true);
  const [showPaste, setShowPaste] = React.useState(false);
  const [pasted, setPasted] = React.useState("");
  const [filter, setFilter] = React.useState("");
  const [open, setOpen] = React.useState<{ path: string; tree: "target" | "gentest"; content: string } | null>(null);
  const [opening, setOpening] = React.useState(false);
  const fileInput = React.useRef<HTMLInputElement>(null);

  // Nice names is literals-only LLM polish — meaningless without the seam.
  React.useEffect(() => {
    if (!useLLM) setNiceNames(false);
  }, [useLLM]);

  const opts: TestRunOptions = {
    useLLM,
    niceNames: useLLM && niceNames,
    staged,
    withLog: !!log && withLog,
  };

  const fixtures = React.useMemo(() => {
    const list = summary?.fixtures ?? [];
    const q = filter.trim().toLowerCase();
    if (!q) return list;
    return list.filter((f) =>
      `${f.service} ${f.layer} ${f.func} ${f.source}`.toLowerCase().includes(q)
    );
  }, [summary, filter]);

  async function openFile(path: string, tree: "target" | "gentest") {
    if (!jobId) return;
    setOpening(true);
    try {
      setOpen({ path, tree, content: await readConvertedFile(jobId, path, tree) });
    } catch (e) {
      setOpen({ path, tree, content: `// ${e instanceof Error ? e.message : "cannot read file"}` });
    } finally {
      setOpening(false);
    }
  }

  function attachFile(f: File | null | undefined) {
    if (!f) return;
    onUploadLog(f);
    if (fileInput.current) fileInput.current.value = "";
  }

  function attachPasted() {
    if (!pasted.trim()) return;
    onUploadLog(new File([pasted], "pasted.log", { type: "text/plain" }));
    setPasted("");
    setShowPaste(false);
  }

  return (
    <div className="space-y-3">
      <Card>
        <CardHeader className="pb-2">
          <div className="flex flex-wrap items-center gap-2">
            <CardTitle className="flex items-center gap-2 text-sm">
              <FlaskConical className="h-4 w-4" />
              Tests — gap report & generation
            </CardTitle>
            <div className="ml-auto flex gap-2">
              <Button size="sm" variant="secondary" disabled={busy || !canRun} onClick={() => onGap(opts)}>
                {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : null}
                Gap report
              </Button>
              <Button size="sm" disabled={busy || !canRun} onClick={() => onGenerate(opts)}>
                {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Play className="h-3.5 w-3.5" />}
                Generate tests
              </Button>
            </div>
          </div>
          <CardDescription>
            {canRun
              ? "Targets the converted Go tree — db stores via sqlmock, handlers via gomock. Attach a runtime log and fixtures come from real values (GT-7)."
              : "gentest targets converted Go trees — convert to Go first."}
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-3">
          {!canRun && onGoConvert && (
            <div className="flex flex-wrap items-center gap-2 rounded-md border border-dashed p-3 text-xs text-muted-foreground">
              <span>gentest targets converted Go trees — convert to Go first.</span>
              <Button size="sm" variant="outline" className="ml-auto" onClick={onGoConvert}>
                Go to Convert
              </Button>
            </div>
          )}

          {/* Runtime log (GT-7): attach the edited app's log for real fixture values. */}
          {canRun && (
            <div className="rounded-md border p-3" aria-label="Runtime log">
              <div className="flex flex-wrap items-center gap-2">
                <FileText className="h-3.5 w-3.5 text-muted-foreground" />
                <span className="text-xs font-medium">Runtime log</span>
                {log ? (
                  <>
                    <Badge variant="secondary">{log.name}</Badge>
                    <span className="text-[11px] text-muted-foreground">{formatBytes(log.bytes)}</span>
                    <label className="ml-1 flex items-center gap-1.5 text-[11px]">
                      <input
                        type="checkbox"
                        className="h-3.5 w-3.5 accent-primary"
                        checked={withLog}
                        onChange={(e) => setWithLog(e.target.checked)}
                      />
                      use log fixtures
                    </label>
                    <Button size="sm" variant="ghost" className="ml-auto h-7 px-2 text-[11px]" disabled={busy} onClick={onClearLog}>
                      <X className="h-3 w-3" /> Remove
                    </Button>
                  </>
                ) : (
                  <>
                    <input
                      ref={fileInput}
                      type="file"
                      accept=".txt,.log,text/plain"
                      className="hidden"
                      aria-label="Attach runtime log"
                      onChange={(e) => attachFile(e.target.files?.[0])}
                    />
                    <Button size="sm" variant="outline" className="h-7 text-[11px]" disabled={busy} onClick={() => fileInput.current?.click()}>
                      <Upload className="h-3 w-3" /> Attach .txt/.log
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
                      className="h-7 text-[11px]"
                      disabled={busy}
                      onClick={() => setShowPaste((v) => !v)}
                    >
                      or paste
                    </Button>
                    <span className="text-[11px] text-muted-foreground">
                      Stays in this job's tmpdir — never uploaded, never bundled in the .zip.
                    </span>
                  </>
                )}
              </div>

              {log && preflight && (
                <div className="mt-2 flex flex-wrap items-center gap-2 text-[11px]">
                  <Badge variant="secondary">{preflight.fromLog} from log</Badge>
                  <Badge variant="outline" className={preflight.assumed > 0 ? "border-amber-500/60 text-amber-600" : ""}>
                    {preflight.assumed} assumed
                  </Badge>
                  <span className="text-muted-foreground">
                    preflight: {preflight.traces} traces ({preflight.complete} complete) · {preflight.lines} lines
                    {preflight.warnings > 0 ? ` · ${preflight.warnings} warnings` : ""}
                  </span>
                </div>
              )}
              {log && !preflight && (
                <p className="mt-2 text-[11px] text-muted-foreground">
                  Run <span className="font-medium">Gap report</span> to preview how many methods this log covers before generating.
                </p>
              )}

              {!log && showPaste && (
                <div className="mt-2 space-y-2">
                  <Textarea
                    rows={5}
                    placeholder="Paste the app's runtime log here (ANSI colors are fine)…"
                    value={pasted}
                    onChange={(e) => setPasted(e.target.value)}
                  />
                  <div className="flex gap-2">
                    <Button size="sm" disabled={busy || !pasted.trim()} onClick={attachPasted}>
                      Attach pasted log
                    </Button>
                    <Button size="sm" variant="ghost" onClick={() => setShowPaste(false)}>
                      Cancel
                    </Button>
                  </div>
                </div>
              )}
            </div>
          )}

          {/* Generate options. */}
          {canRun && (
            <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
              <LLMToggle id="gentest-llm" value={useLLM} onChange={setUseLLM} noKey={!llmKeyOk} disabled={busy} />
              <label className="flex items-center gap-1.5 text-xs">
                <input
                  type="checkbox"
                  className="h-3.5 w-3.5 accent-primary"
                  checked={niceNames}
                  disabled={busy || !useLLM}
                  onChange={(e) => setNiceNames(e.target.checked)}
                />
                Nice names
                <span className="text-[10px] text-muted-foreground">
                  --nice-names · literals-only polish
                  {!useLLM ? " (needs LLM on)" : ""}
                </span>
              </label>
              <label className="flex items-center gap-1.5 text-xs">
                <input
                  type="checkbox"
                  className="h-3.5 w-3.5 accent-primary"
                  checked={staged}
                  disabled={busy}
                  onChange={(e) => setStaged(e.target.checked)}
                />
                Verified snapshot
                <span className="text-[10px] text-muted-foreground">-out · sources + tests, full go test, tree untouched</span>
              </label>
            </div>
          )}

          {/* Requested-vs-effective verdict for the last generate run. */}
          {summary && (
            <div className="flex flex-wrap items-center gap-2 rounded-md border bg-muted/30 p-2 text-[11px]">
              <Badge variant="secondary">{summary.fromLog} from log</Badge>
              <Badge variant="outline" className={summary.assumed > 0 ? "border-amber-500/60 text-amber-600" : ""}>
                {summary.assumed} assumed
              </Badge>
              <Badge variant="outline">{summary.llmCalls} llm calls</Badge>
              {summary.testsFailed ? (
                <Badge variant="destructive" className="gap-1">
                  <AlertTriangle className="h-3 w-3" /> tests FAILED (non-fatal)
                </Badge>
              ) : (
                <Badge variant="secondary" className="gap-1">
                  <CheckCircle2 className="h-3 w-3" /> gates clear
                </Badge>
              )}
              {hasSnapshot && (
                <a className="ml-auto" href={downloadArchiveUrl(jobId ?? "", "gentest")}>
                  <Button size="sm" variant="outline" className="h-7 text-[11px]">
                    <Download className="h-3 w-3" /> Download snapshot .zip
                  </Button>
                </a>
              )}
            </div>
          )}

          {/* Fixture provenance (GT-7). */}
          {summary && summary.fixtures.length > 0 && (
            <div className="space-y-2">
              <div className="flex items-center gap-2">
                <span className="text-xs font-medium">Fixture provenance</span>
                <Input
                  className="h-7 max-w-[220px] text-xs"
                  placeholder="filter service / layer / func…"
                  value={filter}
                  onChange={(e) => setFilter(e.target.value)}
                />
                <span className="text-[11px] text-muted-foreground">
                  {fixtures.length} of {summary.fixtures.length}
                </span>
              </div>
              <ScrollArea className="max-h-[260px] rounded-md border">
                <table className="w-full text-xs">
                  <thead className="text-left text-muted-foreground">
                    <tr className="border-b">
                      <th className="p-2 font-medium">Service</th>
                      <th className="p-2 font-medium">Layer</th>
                      <th className="p-2 font-medium">Function</th>
                      <th className="p-2 font-medium">Fixture source</th>
                    </tr>
                  </thead>
                  <tbody>
                    {fixtures.map((f: GentestFixture) => (
                      <tr key={`${f.layer}/${f.func}`} className="border-b last:border-0">
                        <td className="p-2">{f.service}</td>
                        <td className="p-2 text-muted-foreground">{f.layer}</td>
                        <td className="p-2 font-mono">{f.func}</td>
                        <td className="p-2">
                          <SourceBadge source={f.source} />
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </ScrollArea>
            </div>
          )}

          {/* Written files. */}
          {summary && summary.files.length > 0 && (
            <div className="space-y-1">
              <span className="text-xs font-medium">
                Files {hasSnapshot ? "in the staged snapshot" : "written in place"} ({summary.files.length})
              </span>
              <div className="flex flex-wrap gap-1.5">
                {summary.files.map((f) => (
                  <button
                    key={`${f.tree}:${f.path}`}
                    className="rounded border px-2 py-0.5 font-mono text-[11px] text-muted-foreground hover:bg-accent"
                    disabled={opening}
                    onClick={() => openFile(f.path, f.tree)}
                  >
                    {f.path}
                  </button>
                ))}
              </div>
            </div>
          )}

          {/* Gates. */}
          {summary && summary.gates.length > 0 && (
            <ScrollArea className="max-h-[140px] rounded-md border">
              <ul className="space-y-1 p-2 font-mono text-[11px]">
                {summary.gates.map((g, i) => (
                  <li key={i} className={g.trim().startsWith("FAIL") ? "text-red-600" : "text-muted-foreground"}>
                    {g}
                  </li>
                ))}
              </ul>
            </ScrollArea>
          )}

          <ScrollArea className="max-h-[420px] rounded-md border">
            {busy && !gap ? (
              <div className="space-y-2 p-3" aria-label="Running gentest">
                <span className="flex items-center gap-2 text-xs text-muted-foreground">
                  <Loader2 className="h-3.5 w-3.5 animate-spin" /> Running gentest…
                </span>
                <Skeleton className="h-4 w-full" />
                <Skeleton className="h-4 w-5/6" />
                <Skeleton className="h-4 w-4/6" />
                <Skeleton className="h-24 w-full" />
              </div>
            ) : (
              <pre className="whitespace-pre-wrap p-3 font-mono text-xs">{gap ?? "(no report yet)"}</pre>
            )}
          </ScrollArea>
          {(testFiles?.length ?? 0) > 0 && (
            <p className="text-xs text-muted-foreground">
              {testFiles!.length} test files — {hasSnapshot ? "in the staged snapshot (open above)" : "see the Convert tab tree"}.
            </p>
          )}
        </CardContent>
      </Card>

      {/* Inline viewer for generated / staged files. */}
      {open && (
        <Card>
          <CardHeader className="pb-2">
            <div className="flex items-center gap-2">
              <CardTitle className="text-sm font-mono">{open.path}</CardTitle>
              <Badge variant="outline">{open.tree === "gentest" ? "snapshot" : "converted tree"}</Badge>
              <Button size="sm" variant="ghost" className="ml-auto h-7 px-2" onClick={() => setOpen(null)}>
                <X className="h-3.5 w-3.5" /> Close
              </Button>
            </div>
          </CardHeader>
          <CardContent>
            <CodeView content={open.content} path={open.path} />
          </CardContent>
        </Card>
      )}
    </div>
  );
}
