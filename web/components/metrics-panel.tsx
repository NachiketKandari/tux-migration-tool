"use client";

import * as React from "react";
import { Activity, RefreshCw, Loader2, Database, Files, FlaskConical, PencilLine, GitFork, Hammer, Wrench } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { ScrollArea } from "@/components/ui/scroll-area";
import { fetchMetrics } from "@/lib/api-client";

interface Totals {
  services: number;
  queries: number;
  files: number;
  tests: number;
  mappings: number;
  scenarios: number;
  edits: number;
}

interface RecentEvent {
  ts: string;
  kind: string;
  job: string;
  target?: string;
  queries?: number;
  files?: number;
  tests?: number;
  scenarios?: number;
  note?: string;
}

const CARDS: { key: keyof Totals; label: string; icon: React.ComponentType<{ className?: string }>; hint: string }[] = [
  { key: "queries", label: "Queries converted", icon: Database, hint: "query units across converts" },
  { key: "tests", label: "Tests generated", icon: FlaskConical, hint: "_test.go files via gentest" },
  { key: "services", label: "Services converted", icon: Hammer, hint: "successful convert runs" },
  { key: "files", label: "Files written", icon: Files, hint: "converted + test files" },
  { key: "mappings", label: "Mappings saved", icon: PencilLine, hint: "reviewed mapping saves" },
  { key: "scenarios", label: "Scenarios built", icon: GitFork, hint: "dispatch slices" },
  { key: "edits", label: "File edits", icon: Wrench, hint: "in-viewer code saves" },
];

function timeOf(ts: string): string {
  try {
    return new Date(ts).toLocaleString();
  } catch {
    return ts;
  }
}

function describe(e: RecentEvent): string {
  const bits: string[] = [e.kind];
  if (e.target) bits.push(e.target);
  if (e.queries) bits.push(`${e.queries} q`);
  if (e.tests) bits.push(`${e.tests} t`);
  if (e.files) bits.push(`${e.files} f`);
  if (e.scenarios) bits.push(`${e.scenarios} slices`);
  if (e.note) bits.push(e.note);
  return bits.join(" · ");
}

// Compact one-line master totals for the Overview tab. Self-fetches so every
// tab stays thin; shows a skeleton while loading (never a blank card).
export function MasterTotalsBar({ onView }: { onView?: () => void }) {
  const [totals, setTotals] = React.useState<Totals | null>(null);

  React.useEffect(() => {
    let live = true;
    fetchMetrics()
      .then((d) => {
        if (live) setTotals(d.totals);
      })
      .catch(() => {
        /* offline — bar hides */
      });
    return () => {
      live = false;
    };
  }, []);

  if (!totals) {
    return (
      <div className="flex items-center gap-2" aria-label="Loading master totals">
        <Skeleton className="h-5 w-48" />
      </div>
    );
  }
  const empty = totals.services === 0 && totals.tests === 0 && totals.queries === 0;
  return (
    <button
      onClick={onView}
      title="Open the Metrics tab — cumulative master log"
      className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-md border bg-muted/40 px-3 py-1.5 text-xs text-muted-foreground transition-colors hover:bg-muted"
    >
      <span className="flex items-center gap-1 font-medium text-foreground">
        <Activity className="h-3.5 w-3.5" /> Master log
      </span>
      {empty ? (
        <span>no runs logged yet — convert or generate tests to start the count</span>
      ) : (
        <>
          <span className="tabular-nums">{totals.queries} queries</span>
          <span className="tabular-nums">{totals.tests} tests</span>
          <span className="tabular-nums">{totals.services} services</span>
          <span className="tabular-nums">{totals.files} files</span>
        </>
      )}
    </button>
  );
}

export function MetricsPanel() {
  const [totals, setTotals] = React.useState<Totals | null>(null);
  const [recent, setRecent] = React.useState<RecentEvent[]>([]);
  const [count, setCount] = React.useState(0);
  const [loading, setLoading] = React.useState(true);
  const [err, setErr] = React.useState("");

  const load = React.useCallback(async () => {
    setLoading(true);
    setErr("");
    try {
      const d = await fetchMetrics();
      setTotals(d.totals);
      setRecent(d.recent);
      setCount(d.count);
    } catch (e) {
      setErr(e instanceof Error ? e.message : "cannot load metrics");
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    load();
  }, [load]);

  return (
    <div className="space-y-3">
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4 lg:grid-cols-7">
        {loading && !totals
          ? CARDS.map((c) => (
              <Card key={c.key}>
                <CardContent className="space-y-2 p-4">
                  <Skeleton className="h-3 w-20" />
                  <Skeleton className="h-7 w-12" />
                  <Skeleton className="h-3 w-24" />
                </CardContent>
              </Card>
            ))
          : CARDS.map((c) => (
              <Card key={c.key}>
                <CardContent className="p-4">
                  <div className="flex items-center gap-1.5 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
                    <c.icon className="h-3.5 w-3.5" />
                    {c.label}
                  </div>
                  <p className="mt-1 font-mono text-xl font-bold tabular-nums">{totals?.[c.key] ?? 0}</p>
                  <p className="truncate text-[11px] text-muted-foreground" title={c.hint}>
                    {c.hint}
                  </p>
                </CardContent>
              </Card>
            ))}
      </div>

      <Card>
        <CardHeader className="pb-2">
          <div className="flex flex-wrap items-center gap-2">
            <CardTitle className="flex items-center gap-2 text-sm">
              <Activity className="h-4 w-4" />
              Recent runs
            </CardTitle>
            {count > 0 && <Badge variant="secondary">{count} events</Badge>}
            <Button size="sm" variant="ghost" className="ml-auto" disabled={loading} onClick={load}>
              {loading ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <RefreshCw className="h-3.5 w-3.5" />}
              Refresh
            </Button>
          </div>
          <CardDescription>
            Append-only master log at <code className="font-mono">conversion_logs/web-metrics.jsonl</code> — every
            upload, convert, gentest, mapping save, and in-viewer edit adds a line. Future pipeline steps add their own
            counters the same way.
          </CardDescription>
        </CardHeader>
        <CardContent>
          {err ? (
            <p className="py-4 text-center text-xs text-destructive">{err}</p>
          ) : loading && recent.length === 0 ? (
            <div className="space-y-2" aria-label="Loading recent runs">
              {[0, 1, 2].map((i) => (
                <Skeleton key={i} className="h-8 w-full" />
              ))}
            </div>
          ) : recent.length === 0 ? (
            <p className="rounded-md border border-dashed p-4 text-center text-xs text-muted-foreground">
              No runs logged yet — upload a file, convert it, or generate tests and the totals will accumulate here.
            </p>
          ) : (
            <ScrollArea className="max-h-[320px]">
              <ul className="divide-y">
                {recent.map((e, i) => (
                  <li key={`${e.ts}-${i}`} className="flex flex-wrap items-center gap-2 py-1.5 text-xs">
                    <Badge variant="outline" className="font-mono">
                      {e.kind}
                    </Badge>
                    <span className="truncate font-mono" title={e.job}>
                      {e.job}
                    </span>
                    <span className="text-muted-foreground">{describe(e)}</span>
                    <span className="ml-auto shrink-0 tabular-nums text-muted-foreground">{timeOf(e.ts)}</span>
                  </li>
                ))}
              </ul>
            </ScrollArea>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
