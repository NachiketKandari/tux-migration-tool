"use client";

import * as React from "react";
import { GitFork, Play, Loader2, FlaskConical } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { ScrollArea } from "@/components/ui/scroll-area";
import { CodeView } from "@/components/files";
import type { ScenarioBundle } from "@/lib/jobs";
import { previewFilter, type FilterPreview } from "@/lib/api-client";

function parseScenarioName(name: string): { key: string; value: string } {
  // SVC_X.trn_cd_A.pc → key "trn_cd=A"
  const base = name.replace(/\.pc$/i, "");
  const dot = base.indexOf(".");
  const rest = dot >= 0 ? base.slice(dot + 1) : base;
  const us = rest.lastIndexOf("_");
  if (us > 0) return { key: `${rest.slice(0, us)}=${rest.slice(us + 1)}`, value: rest.slice(us + 1) };
  return { key: rest, value: rest };
}

function formatBlocks(blocks?: [number, number][]): string {
  if (!blocks || blocks.length === 0) return "none";
  return blocks.map(([a, b]) => `${a}-${b}`).join(", ");
}

function Playground({ jobId }: { jobId: string }) {
  const [expr, setExpr] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [raw, setRaw] = React.useState("");
  const [previews, setPreviews] = React.useState<FilterPreview[]>([]);
  const [selEntry, setSelEntry] = React.useState<string | null>(null);
  const [err, setErr] = React.useState("");

  const preview: FilterPreview | undefined = React.useMemo(() => {
    if (previews.length === 0) return undefined;
    if (previews.length === 1) return previews[0];
    return previews.find((p) => p.entry === selEntry) ?? previews[0];
  }, [previews, selEntry]);

  React.useEffect(() => {
    if (previews.length > 0 && !selEntry) setSelEntry(previews[0].entry);
  }, [previews, selEntry]);

  async function run() {
    const text = expr.trim();
    if (!text || busy) return;
    setBusy(true);
    setErr("");
    try {
      const d = await previewFilter(jobId, text);
      setRaw(d.output ?? "");
      const list = d.previews ?? (d.preview ? [d.preview] : []);
      setPreviews(list);
      setSelEntry(list[0]?.entry ?? null);
      if (list.length === 0 && (d.code ?? 0) !== 0) {
        setErr(d.output || "preview failed — see Logs");
      }
    } catch (e) {
      setErr(e instanceof Error ? e.message : "preview failed");
      setPreviews([]);
      setRaw("");
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="flex items-center gap-2 text-sm">
          <FlaskConical className="h-4 w-4" />
          scenarioFilter playground
        </CardTitle>
        <CardDescription>
          Fold one endpoint over several arms, e.g. <code className="font-mono">c_flag == &apos;F&apos; || c_flag == &apos;I&apos;</code>.
          Backend fold — read-only preview with flattened source, no artifacts written.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-2">
        <div className="flex gap-2">
          <Input
            value={expr}
            onChange={(e) => setExpr(e.target.value)}
            placeholder="c_flag == 'F' || c_flag == 'I'"
            className="font-mono text-xs"
            aria-label="scenarioFilter expression"
            onKeyDown={(e) => {
              if (e.key === "Enter") run();
            }}
          />
          <Button size="sm" disabled={busy || !expr.trim()} onClick={run}>
            {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Play className="h-3.5 w-3.5" />}
            Preview
          </Button>
        </div>
        {err && (
          <p className="rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs" role="alert">
            {err}
          </p>
        )}
        {previews.length > 1 && (
          <div className="flex flex-wrap gap-1.5">
            {previews.map((p) => (
              <Button
                key={p.entry}
                size="sm"
                variant={preview?.entry === p.entry ? "default" : "outline"}
                className="h-7 font-mono text-[11px]"
                onClick={() => setSelEntry(p.entry)}
              >
                {p.entry}
                {p.error ? " · error" : ""}
              </Button>
            ))}
          </div>
        )}
        {preview?.error && (
          <p className="rounded-md border border-amber-500/30 bg-amber-500/10 p-2 text-xs" role="alert">
            {preview.entry}: {preview.error}
          </p>
        )}
        {preview && !preview.error && (
          <div className="space-y-2">
            <div className="flex flex-wrap gap-1.5" aria-live="polite">
              <Badge variant="secondary" className="font-mono text-[11px]">
                {preview.mergedKey || preview.filter}
              </Badge>
              <Badge variant="outline" className="tabular-nums">
                {(preview.matched ?? []).length} matched
              </Badge>
              <Badge variant="outline" className="tabular-nums">
                {preview.blockLines ?? 0} lines · {formatBlocks(preview.blocks)}
              </Badge>
              <Badge variant="outline" className="tabular-nums">
                kept {preview.kept ?? 0} · dropped {preview.dropped ?? 0}
                {(preview.unfolded ?? 0) > 0 ? ` · unfolded ${preview.unfolded}` : ""}
              </Badge>
              {(preview.queries ?? []).length > 0 && (
                <Badge variant="outline" className="font-mono text-[11px]">
                  {(preview.queries ?? []).length} queries: {(preview.queries ?? []).join(", ")}
                </Badge>
              )}
              {preview.logicOnly && <Badge variant="outline">logic-only — no FML traffic</Badge>}
            </div>
            {(preview.matched ?? []).length > 0 && (
              <p className="font-mono text-[11px] text-muted-foreground">
                matched: {(preview.matched ?? []).join(", ")}
                {(preview.pruned ?? []).length > 0 && <> · pruned: {(preview.pruned ?? []).join(", ")}</>}
              </p>
            )}
            <p className="font-mono text-[11px] text-muted-foreground">
              reads: {(preview.reads ?? []).join(", ") || "none"} · writes: {(preview.writes ?? []).join(", ") || "none"}
              {(preview.tx ?? []).length > 0 && <> · tx: {(preview.tx ?? []).join(", ")}</>}
              {(preview.residue ?? []).length > 0 && <> · residue: {(preview.residue ?? []).join("; ")}</>}
            </p>
            {preview.flattened && (
              <ScrollArea className="max-h-[420px]">
                <CodeView content={preview.flattened} path={`${preview.entry}.filter.pc`} />
              </ScrollArea>
            )}
            {raw && (
              <details className="text-[11px] text-muted-foreground">
                <summary className="cursor-pointer font-mono">console output</summary>
                <ScrollArea className="max-h-[180px]">
                  <CodeView content={raw} path="filter-preview.log" />
                </ScrollArea>
              </details>
            )}
          </div>
        )}
        {!preview && raw && !err && (
          <ScrollArea className="max-h-[240px]">
            <CodeView content={raw} path="filter-preview.log" />
          </ScrollArea>
        )}
      </CardContent>
    </Card>
  );
}

export function ScenarioExplorer({
  jobId,
  bundle,
  busy,
  onBuild,
}: {
  jobId: string;
  bundle?: ScenarioBundle;
  busy: boolean;
  onBuild: () => void;
}) {
  const [sel, setSel] = React.useState<string | null>(null);

  React.useEffect(() => {
    if (bundle && bundle.artifacts.length > 0 && !sel) setSel(bundle.artifacts[0].name);
  }, [bundle, sel]);

  if (!bundle) {
    return (
      <div className="space-y-3">
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-sm">
              <GitFork className="h-4 w-4" />
              Scenarios — one slice per dispatch value
            </CardTitle>
            <CardDescription>
              The entry folds on its dispatch axis (e.g. trn_cd). Each arm becomes an isolated slice with its own
              FML census and queries — the unit a mapping endpoint covers.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <Button size="sm" disabled={busy} onClick={onBuild}>
              {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Play className="h-3.5 w-3.5" />}
              Build scenarios
            </Button>
          </CardContent>
        </Card>
        <Playground jobId={jobId} />
      </div>
    );
  }

  const selected = bundle.artifacts.find((a) => a.name === sel);

  return (
    <div className="space-y-3">
      {busy && (
        <div className="flex items-center gap-2 rounded-md border bg-muted/40 px-3 py-2 text-xs text-muted-foreground" aria-live="polite">
          <Loader2 className="h-3.5 w-3.5 animate-spin" /> Rebuilding scenarios…
        </div>
      )}
      <Card>
        <CardHeader className="pb-2">
          <div className="flex flex-wrap items-center gap-2">
            <CardTitle className="flex items-center gap-2 text-sm">
              <GitFork className="h-4 w-4" />
              Dispatch axis
            </CardTitle>
            <Badge variant="secondary">{bundle.artifacts.length} slices</Badge>
            <Button size="sm" variant="outline" className="ml-auto" disabled={busy} onClick={onBuild}>
              {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Play className="h-3.5 w-3.5" />}
              Rebuild
            </Button>
          </div>
          {bundle.note && <CardDescription>{bundle.note}</CardDescription>}
        </CardHeader>
        <CardContent>
          <ScrollArea className="max-h-[220px]">
            <CodeView content={bundle.axesText || "(no axes output)"} path="axes.txt" />
          </ScrollArea>
        </CardContent>
      </Card>

      {bundle.artifacts.length > 0 && (
        <div className="grid grid-cols-1 gap-3 md:grid-cols-[260px_1fr]">
          <Card>
            <CardHeader className="pb-2">
              <CardTitle className="text-sm">Slices</CardTitle>
              <CardDescription>Pick a value to see its flattened source.</CardDescription>
            </CardHeader>
            <CardContent className="flex flex-wrap gap-1.5 md:flex-col md:items-stretch">
              {bundle.artifacts.map((a) => {
                const { key } = parseScenarioName(a.name);
                const active = sel === a.name;
                return (
                  <button
                    key={a.name}
                    onClick={() => setSel(a.name)}
                    className={`rounded-md border px-2.5 py-1.5 text-left font-mono text-xs transition-colors ${
                      active ? "border-primary bg-primary text-primary-foreground" : "hover:bg-accent"
                    }`}
                    title={a.name}
                  >
                    {key}
                  </button>
                );
              })}
            </CardContent>
          </Card>
          <Card>
            <CardHeader className="pb-2">
              <CardTitle className="font-mono text-sm">{selected?.name}</CardTitle>
              <CardDescription>Flattened .pc — shared init, this arm&apos;s folded body, the shared tail, and any common local functions; a brace-balanced fragment, not a compilable function, with per-line source provenance.</CardDescription>
            </CardHeader>
            <CardContent>
              <ScrollArea className="max-h-[420px]">
                {selected ? (
                  <CodeView content={selected.content} path={selected.name} />
                ) : (
                  <p className="p-3 text-xs text-muted-foreground">Select a slice.</p>
                )}
              </ScrollArea>
            </CardContent>
          </Card>
        </div>
      )}

      {bundle.sharedMd && (
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm">Shared vs unique blocks</CardTitle>
            <CardDescription>What every slice shares, and what makes each arm unique.</CardDescription>
          </CardHeader>
          <CardContent>
            <ScrollArea className="max-h-[280px]">
              <CodeView content={bundle.sharedMd} path="shared.md" />
            </ScrollArea>
          </CardContent>
        </Card>
      )}

      <Playground jobId={jobId} />
    </div>
  );
}
