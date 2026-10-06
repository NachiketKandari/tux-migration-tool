"use client";

import * as React from "react";
import {
  ArrowRight,
  ArrowLeftRight,
  Database,
  FileCode2,
  FunctionSquare,
  GitBranch,
  Loader2,
  MessageSquareText,
  Search,
  Waypoints,
} from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";
import type { Confidence, LineageNode, SourceKind } from "@/lib/lineage";
import type { LineageState } from "@/hooks/use-lineage";

const KIND_ICON: Record<SourceKind, React.ComponentType<{ className?: string }>> = {
  condition: GitBranch,
  query: Database,
  fml: MessageSquareText,
  tpcall: ArrowLeftRight,
  function: FunctionSquare,
};

const KIND_LABEL: Record<SourceKind, string> = {
  condition: "Branches",
  query: "Queries",
  fml: "FML fields",
  tpcall: "Tpcalls",
  function: "Functions",
};

type KindFilter = "all" | SourceKind;

const FILTERS: { id: KindFilter; label: string }[] = [
  { id: "all", label: "All" },
  { id: "condition", label: "Branches" },
  { id: "query", label: "Queries" },
  { id: "fml", label: "FML" },
  { id: "tpcall", label: "Tpcalls" },
];

function confVariant(c: Confidence): "default" | "secondary" | "outline" {
  if (c === "exact") return "default";
  if (c === "derived") return "secondary";
  return "outline";
}

export function LineageExplorer({
  lineage,
  loading,
  error,
  onOpenFile,
}: {
  lineage: LineageState | null;
  loading: boolean;
  error: string;
  onOpenFile: (path: string) => void;
}) {
  const [filter, setFilter] = React.useState<KindFilter>("all");
  const [q, setQ] = React.useState("");
  const [selId, setSelId] = React.useState<string | null>(null);

  const nodes = React.useMemo(() => lineage?.nodes ?? [], [lineage]);

  const visible = React.useMemo(() => {
    const needle = q.trim().toLowerCase();
    return nodes.filter((n) => {
      if (filter !== "all" && n.kind !== filter) return false;
      if (n.kind === "function" && filter === "all" && !needle) return false; // functions are context — opt-in
      if (!needle) return true;
      return `${n.id} ${n.label} ${n.detail} ${n.meta} ${n.targets.map((t) => `${t.file} ${t.reason}`).join(" ")}`
        .toLowerCase()
        .includes(needle);
    });
  }, [nodes, filter, q]);

  // Keep selection valid across refetches / filters.
  React.useEffect(() => {
    if (!lineage) {
      setSelId(null);
      return;
    }
    if (!selId || !nodes.some((n) => n.id === selId)) {
      const first = nodes.find((n) => n.targets.length > 0) ?? nodes[0] ?? null;
      setSelId(first?.id ?? null);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [lineage]);

  const selected: LineageNode | null = React.useMemo(
    () => nodes.find((n) => n.id === selId) ?? null,
    [nodes, selId]
  );

  const stats = React.useMemo(() => {
    const withTargets = nodes.filter((n) => n.targets.length > 0).length;
    const exact = nodes.reduce((a, n) => a + n.targets.filter((t) => t.confidence === "exact").length, 0);
    const verified = nodes.reduce((a, n) => a + n.targets.filter((t) => t.verified).length, 0);
    return {
      parts: nodes.length,
      withTargets,
      exact,
      verified,
      files: lineage?.files.length ?? 0,
      ledgerLinks: lineage?.ledgerLinks ?? 0,
    };
  }, [nodes, lineage]);

  const counts = React.useMemo(() => {
    const m = new Map<SourceKind, number>();
    for (const n of nodes) m.set(n.kind, (m.get(n.kind) ?? 0) + 1);
    return m;
  }, [nodes]);

  if (loading && !lineage) {
    return (
      <div className="space-y-2" aria-label="Loading trace">
        <div className="flex gap-2">
          <Skeleton className="h-8 w-48" />
          <Skeleton className="h-8 w-24" />
        </div>
        <div className="grid grid-cols-1 gap-3 lg:grid-cols-[minmax(0,5fr)_64px_minmax(0,6fr)]">
          <Skeleton className="h-64 w-full" />
          <Skeleton className="hidden h-64 lg:block" />
          <Skeleton className="h-64 w-full" />
        </div>
      </div>
    );
  }

  if (error && !lineage) {
    return (
      <Card>
        <CardContent className="p-6 text-center text-sm text-muted-foreground">
          {error.includes("convert first") ? (
            <>
              <Waypoints className="mx-auto mb-2 h-6 w-6" />
              <p className="font-medium text-foreground">No converted tree yet</p>
              <p className="mt-1 text-xs">Run Step 2 (Convert) and this tab will show exactly which source part landed in which file.</p>
            </>
          ) : (
            <p>{error}</p>
          )}
        </CardContent>
      </Card>
    );
  }

  if (!lineage || nodes.length === 0) {
    return (
      <Card>
        <CardContent className="p-6 text-center text-sm text-muted-foreground">
          <Waypoints className="mx-auto mb-2 h-6 w-6" />
          <p>Nothing to trace yet — upload a file and convert it.</p>
        </CardContent>
      </Card>
    );
  }

  return (
    <div className="space-y-3">
      {/* Controls */}
      <Card>
        <CardHeader className="pb-2">
          <div className="flex flex-wrap items-center gap-2">
            <CardTitle className="flex items-center gap-2 text-sm">
              <Waypoints className="h-4 w-4" />
              Trace — this part became that part
            </CardTitle>
            <Badge variant="secondary" className="tabular-nums" title={stats.verified > 0 ? `${stats.verified} ledger-verified links (backend ground truth)` : "No ledger links — heuristic token matching only"}>
              {stats.withTargets}/{stats.parts} linked · {stats.exact} exact
              {stats.verified > 0 && <> · {stats.verified} verified</>} · {stats.files} files
            </Badge>
            <div className="relative ml-auto w-full sm:w-56">
              <Search className="absolute left-2 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input
                value={q}
                onChange={(e) => setQ(e.target.value)}
                placeholder="Filter parts, files, reasons…"
                aria-label="Filter trace"
                className="h-8 pl-7 text-xs"
              />
            </div>
          </div>
          <CardDescription>
            Pick a source part on the left — the right shows every generated file it landed in, with the matching
            evidence. <span className="font-medium text-foreground">Verified</span> links come from the backend
            ledger (source lines → file); the rest are heuristic token matches. Click a file to open it in the
            Convert tab. Evidence snippets wrap by default.
          </CardDescription>
        </CardHeader>
        <CardContent className="flex flex-wrap gap-1.5">
          {FILTERS.map((f) => {
            const active = filter === f.id;
            const n = f.id === "all" ? nodes.length : (counts.get(f.id as SourceKind) ?? 0);
            return (
              <button
                key={f.id}
                onClick={() => setFilter(f.id)}
                aria-pressed={active}
                className={cn(
                  "rounded-full border px-2.5 py-1 text-xs font-medium tabular-nums transition-all duration-200",
                  active
                    ? "border-primary bg-primary text-primary-foreground shadow-sm"
                    : "border-input bg-background text-muted-foreground hover:bg-accent hover:text-foreground"
                )}
              >
                {f.label} · {n}
              </button>
            );
          })}
          {filter === "all" && (counts.get("function") ?? 0) > 0 && (
            <button
              onClick={() => setQ(q ? q : "fn:")}
              className="rounded-full border border-dashed px-2.5 py-1 text-xs text-muted-foreground transition-colors hover:bg-accent"
              title="Show structural function context"
            >
              + {(counts.get("function") ?? 0)} functions hidden
            </button>
          )}
        </CardContent>
      </Card>

      {/* Three-pane trace */}
      <div className="grid grid-cols-1 gap-3 lg:grid-cols-[minmax(0,5fr)_64px_minmax(0,6fr)]">
        {/* Source parts */}
        <Card className="min-w-0">
          <CardHeader className="pb-2">
            <CardTitle className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
              Source parts · {visible.length}
            </CardTitle>
          </CardHeader>
          <CardContent className="p-2 pt-0">
            <ScrollArea className="max-h-[52vh] min-h-[280px] border-0 bg-transparent">
              {visible.length === 0 ? (
                <p className="p-4 text-center text-xs text-muted-foreground">
                  No parts match{counts.get("function") ? " — functions hide under “All” until you search." : "."}
                </p>
              ) : (
                <ul className="space-y-1.5 p-1">
                  {visible.slice(0, 150).map((n, i) => {
                    const Icon = KIND_ICON[n.kind];
                    const active = n.id === selId;
                    return (
                      <li key={n.id} className="trace-item" style={{ animationDelay: `${Math.min(i, 14) * 24}ms` }}>
                        <button
                          onClick={() => setSelId(n.id)}
                          aria-current={active}
                          aria-label={`${n.kind} ${n.label}, ${n.targets.length} targets`}
                          className={cn(
                            "group flex w-full items-start gap-2 rounded-lg border p-2.5 text-left transition-all duration-200",
                            active
                              ? "border-primary bg-primary/5 shadow-sm ring-1 ring-primary/40"
                              : "border-transparent bg-muted/30 hover:-translate-y-px hover:border-primary/30 hover:bg-accent/60 hover:shadow-sm"
                          )}
                        >
                          <span
                            className={cn(
                              "mt-0.5 flex h-7 w-7 shrink-0 items-center justify-center rounded-md transition-colors",
                              active ? "bg-primary text-primary-foreground" : "bg-muted text-muted-foreground group-hover:bg-primary/10 group-hover:text-foreground"
                            )}
                          >
                            <Icon className="h-3.5 w-3.5" />
                          </span>
                          <span className="min-w-0 flex-1">
                            <span className="flex items-center gap-1.5">
                              <span className="truncate font-mono text-xs font-semibold">{n.label}</span>
                              <Badge variant={n.targets.length ? "secondary" : "outline"} className="shrink-0 px-1.5 py-0 text-[10px] tabular-nums">
                                → {n.targets.length}
                              </Badge>
                            </span>
                            <span className="mt-0.5 block truncate font-mono text-[11px] text-muted-foreground" title={n.detail}>
                              {n.detail}
                            </span>
                            {n.meta && (
                              <span className="mt-0.5 block text-[11px] tabular-nums text-muted-foreground">{n.meta}</span>
                            )}
                          </span>
                          <ArrowRight
                            className={cn(
                              "mt-1.5 h-3.5 w-3.5 shrink-0 transition-all duration-200",
                              active ? "translate-x-0 text-primary opacity-100" : "-translate-x-1 opacity-0 group-hover:translate-x-0 group-hover:opacity-60"
                            )}
                          />
                        </button>
                      </li>
                    );
                  })}
                </ul>
              )}
              {visible.length > 150 && (
                <p className="p-2 text-center text-[11px] text-muted-foreground">Showing 150 of {visible.length} — refine the filter.</p>
              )}
            </ScrollArea>
          </CardContent>
        </Card>

        {/* Connector */}
        <div className="hidden items-stretch justify-center lg:flex" aria-hidden>
          <div className="trace-connector relative flex w-full flex-col items-center justify-center">
            <span className="mb-1 rounded-full border bg-background px-2 py-0.5 font-mono text-[10px] tabular-nums text-muted-foreground shadow-sm">
              {selected ? `×${selected.targets.length}` : "×0"}
            </span>
            <span className={cn("text-2xl leading-none transition-all duration-300", selected?.targets.length ? "text-primary" : "text-muted-foreground/40")}>
              →
            </span>
            <span className="mt-1 font-mono text-[10px] uppercase tracking-widest text-muted-foreground/70">became</span>
          </div>
        </div>

        {/* Targets */}
        <Card className="min-w-0 border-primary/20">
          <CardHeader className="pb-2">
            <CardTitle className="flex min-w-0 items-center gap-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
              <FileCode2 className="h-3.5 w-3.5 shrink-0" />
              <span className="truncate">
                {selected ? (
                  <>Became — <span className="font-mono normal-case text-foreground">{selected.label}</span></>
                ) : (
                  "Became"
                )}
              </span>
            </CardTitle>
          </CardHeader>
          <CardContent className="p-2 pt-0">
            <ScrollArea className="max-h-[52vh] min-h-[280px] border-0 bg-transparent">
              {!selected ? (
                <p className="p-4 text-center text-xs text-muted-foreground">Select a source part.</p>
              ) : selected.targets.length === 0 ? (
                <div className="space-y-2 p-3 text-xs text-muted-foreground">
                  <p className="font-medium text-foreground">No generated file references this part.</p>
                  <p>
                    That is expected when the converter folds it away: dropped session fields, default-arm fallthrough,
                    or a helper the deterministic draft defers with a <code className="font-mono">tuxconv:TODO</code>.
                    Check the Convert summary and the file tree for the TODO marker.
                  </p>
                  <p className="font-mono text-[11px]">{selected.detail}</p>
                </div>
              ) : (
                <ul key={selected.id} className="space-y-2 p-1">
                  {selected.targets.map((t) => (
                    <li
                      key={t.file}
                      className="trace-item rounded-lg border bg-card p-2.5 shadow-sm transition-all duration-200 hover:-translate-y-px hover:border-primary/40 hover:shadow"
                    >
                      <div className="flex flex-wrap items-center gap-1.5">
                        <Badge variant="secondary" className="font-mono text-[10px]">
                          {t.layer}
                        </Badge>
                        <Badge variant={confVariant(t.confidence)} className="text-[10px]">
                          {t.confidence}
                        </Badge>
                        {t.verified && (
                          <Badge variant="default" className="text-[10px]" title="Backend ledger ground truth (source lines → file)">
                            verified
                          </Badge>
                        )}
                        <code className="min-w-0 flex-1 truncate font-mono text-[11px] font-semibold" title={t.file}>
                          {t.file}
                        </code>
                      </div>
                      <p className="mt-1 text-[11px] text-muted-foreground">{t.reason}</p>
                      {t.snippet && (
                        <pre className="mt-1.5 overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted/50 p-2 font-mono text-[11px] leading-4">
                          {t.snippet}
                        </pre>
                      )}
                      <Button size="sm" variant="ghost" className="mt-1 h-7 px-2 text-xs" onClick={() => onOpenFile(t.file)}>
                        Open in Convert <ArrowRight className="h-3 w-3" />
                      </Button>
                    </li>
                  ))}
                </ul>
              )}
            </ScrollArea>
          </CardContent>
        </Card>
      </div>

      {/* Scaffolding footnote */}
      {(lineage.unmappedFiles?.length ?? 0) > 0 && (
        <p className="px-1 text-[11px] text-muted-foreground">
          {lineage.unmappedFiles.length} shared-scaffolding file{lineage.unmappedFiles.length === 1 ? "" : "s"} with no
          single owner (interfaces, routers, placeholders):{" "}
          <span className="font-mono">{lineage.unmappedFiles.slice(0, 6).join(", ")}</span>
          {lineage.unmappedFiles.length > 6 && <> +{lineage.unmappedFiles.length - 6} more</>} — open them in Convert.
        </p>
      )}

      {/* Group census */}
      <div className="flex flex-wrap gap-1.5 px-1">
        {(Object.keys(KIND_LABEL) as SourceKind[]).map((k) => (
          <Badge key={k} variant="outline" className="tabular-nums">
            {KIND_LABEL[k]} · {counts.get(k) ?? 0}
          </Badge>
        ))}
      </div>
    </div>
  );
}
