"use client";

import * as React from "react";
import { GitBranch, Database, FunctionSquare, MessageSquareText, Gauge, Waypoints } from "lucide-react";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Progress } from "@/components/ui/progress";
import { asArray, countKind, entryOf } from "@/lib/ir";
import { MasterTotalsBar } from "@/components/metrics-panel";
import type { FlowReport } from "@/lib/jobs";

const CHART_COLORS = [
  "hsl(var(--chart-1))",
  "hsl(var(--chart-2))",
  "hsl(var(--chart-3))",
  "hsl(var(--chart-4))",
  "hsl(var(--chart-5))",
];

function Donut({ data }: { data: [string, number][] }) {
  const total = data.reduce((a, [, v]) => a + v, 0);
  const denom = total || 1;
  let acc = 0;
  const segs = data.slice(0, 5).map(([k, v], i) => {
    const from = (acc / denom) * 100;
    acc += v;
    const to = (acc / denom) * 100;
    return { k, v, from, to, color: CHART_COLORS[i % CHART_COLORS.length] };
  });
  const R = 42;
  const C = 2 * Math.PI * R;
  return (
    <div className="flex items-center gap-4">
      <svg width="110" height="110" viewBox="0 0 110 110" role="img" aria-label="Query kind breakdown">
        <circle cx="55" cy="55" r={R} fill="none" stroke="hsl(var(--muted))" strokeWidth="14" />
        {segs.map((s) => {
          const len = ((s.to - s.from) / 100) * C;
          const off = (s.from / 100) * C;
          return (
            <circle
              key={s.k}
              cx="55"
              cy="55"
              r={R}
              fill="none"
              stroke={s.color}
              strokeWidth="14"
              strokeDasharray={`${len} ${C - len}`}
              strokeDashoffset={-off}
              transform="rotate(-90 55 55)"
              strokeLinecap="butt"
            />
          );
        })}
        <text x="55" y="52" textAnchor="middle" className="fill-foreground text-lg font-bold">
          {total}
        </text>
        <text x="55" y="66" textAnchor="middle" className="fill-muted-foreground text-[10px]">
          queries
        </text>
      </svg>
      <ul className="space-y-1 text-xs">
        {segs.map((s) => (
          <li key={s.k} className="flex items-center gap-2">
            <span className="h-2.5 w-2.5 rounded-sm" style={{ background: s.color }} />
            <span className="font-mono">{s.k}</span>
            <span className="text-muted-foreground tabular-nums">×{s.v}</span>
          </li>
        ))}
        {segs.length === 0 && <li className="text-muted-foreground">No queries.</li>}
      </ul>
    </div>
  );
}

function Bars({ data, label }: { data: [string, number][]; label: string }) {
  const max = Math.max(1, ...data.map(([, v]) => v));
  return (
    <div>
      <p className="mb-2 text-xs font-medium text-muted-foreground">{label}</p>
      <div className="space-y-1.5">
        {data.slice(0, 6).map(([k, v], i) => (
          <div key={k} className="flex items-center gap-2 text-xs">
            <span className="w-28 truncate font-mono" title={k}>
              {k}
            </span>
            <div className="h-2 flex-1 overflow-hidden rounded-full bg-muted">
              <div
                className="h-full rounded-full"
                style={{ width: `${(v / max) * 100}%`, background: CHART_COLORS[i % CHART_COLORS.length] }}
              />
            </div>
            <span className="w-8 text-right tabular-nums text-muted-foreground">{v}</span>
          </div>
        ))}
        {data.length === 0 && <p className="text-xs text-muted-foreground">Nothing to chart.</p>}
      </div>
    </div>
  );
}

export function OverviewDashboard({ ir, irList, inputFiles, analysis, flowReport, onGoMetrics, onGoTrace, onGoAnalyze }: { ir?: Record<string, unknown>; irList?: { name: string; ir: Record<string, unknown> }[]; inputFiles?: string[]; analysis?: { complexity: string }[]; flowReport?: FlowReport; onGoMetrics?: () => void; onGoTrace?: () => void; onGoAnalyze?: () => void }) {
  const mergedIr = React.useMemo(() => {
    if (!irList || irList.length === 0) return ir;
    // Aggregate batch IRs so the KPIs read across the whole upload.
    const get = (o: Record<string, unknown>, keys: string[]): unknown[] => {
      for (const k of keys) if (Array.isArray(o[k])) return o[k] as unknown[];
      return [];
    };
    const pick = (...keys: string[][]): unknown[] => irList.flatMap((e) => get(e.ir, keys.flat()));
    return {
      ...(ir ?? {}),
      conditions: pick(["conditions", "Conditions"]),
      queries: pick(["queries", "Queries", "queryUnits", "QueryUnits"]),
      functions: pick(["functions", "Functions"]),
      fml_ops: pick(["fml_ops", "fmlOps", "fml", "FmlOps"]),
    } as Record<string, unknown>;
  }, [ir, irList]);
  const conditions = asArray(mergedIr, ["conditions", "Conditions"]);
  const queries = asArray(mergedIr, ["queries", "Queries", "queryUnits", "QueryUnits"]);
  const functions = asArray(mergedIr, ["functions", "Functions"]);
  const fmlOps = asArray(mergedIr, ["fml_ops", "fmlOps", "fml", "FmlOps"]);

  const queryKinds = React.useMemo(
    () => Array.from(countKind(queries, ["kind", "Kind", "op", "queryKind", "QueryKind", "type"]).entries()).sort((a, b) => b[1] - a[1]),
    [queries]
  );
  const fmlKinds = React.useMemo(
    () => Array.from(countKind(fmlOps, ["kind", "Kind", "op", "Op"]).entries()).sort((a, b) => b[1] - a[1]),
    [fmlOps]
  );

  const coverage = React.useMemo(() => {
    const fns = flowReport?.files.flatMap((f) => f.functions) ?? [];
    const cls = fns.reduce((a, f) => a + (f.coverage?.classified ?? 0), 0);
    const total = fns.reduce((a, f) => a + (f.coverage?.codeLines ?? 0), 0);
    return { cls, total, pct: total ? Math.round((cls / total) * 100) : 100 };
  }, [flowReport]);

  const kpis = [
    { icon: GitBranch, label: inputFiles && inputFiles.length > 1 ? `Files (${inputFiles.length})` : "Entry", value: inputFiles && inputFiles.length > 1 ? String(inputFiles.length) : entryOf(mergedIr), sub: `${conditions.length} conditions` },
    { icon: Database, label: "Query units", value: String(queries.length), sub: `${queryKinds.length} kinds` },
    { icon: FunctionSquare, label: "Functions", value: String(functions.length), sub: "inventoried" },
    { icon: MessageSquareText, label: "FML ops", value: String(fmlOps.length), sub: `${fmlKinds.length} op kinds` },
    {
      icon: Gauge,
      label: analysis ? `Triage (${analysis.length})` : "Flow coverage",
      value: analysis && analysis.length > 0 ? `${analysis.filter((a) => a.complexity.toUpperCase() === "HIGH").length} HIGH` : coverage.total ? `${coverage.pct}%` : "—",
      sub: analysis && analysis.length > 0 ? "see Analyze step" : coverage.total ? `${coverage.cls}/${coverage.total} lines` : "run flow for detail",
    },
  ];

  return (
    <div className="space-y-4">
      <MasterTotalsBar onView={onGoMetrics} />
      {inputFiles && inputFiles.length > 1 && (
        <Card>
          <CardContent className="flex flex-wrap items-center gap-1.5 p-3">
            <span className="text-xs font-medium">Batch:</span>
            {inputFiles.slice(0, 6).map((n) => (
              <Badge key={n} variant="secondary" className="max-w-[200px] truncate font-mono text-[10px]" title={n}>
                {n}
              </Badge>
            ))}
            {inputFiles.length > 6 && <Badge variant="outline">+{inputFiles.length - 6} more</Badge>}
            {onGoAnalyze && (
              <Button size="sm" variant="ghost" className="ml-auto h-7 px-2 text-xs" onClick={onGoAnalyze}>
                Effort estimate →
              </Button>
            )}
          </CardContent>
        </Card>
      )}
      <div className="grid grid-cols-2 gap-3 md:grid-cols-5">
        {kpis.map((k) => (
          <Card key={k.label}>
            <CardContent className="p-4">
              <div className="flex items-center gap-1.5 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
                <k.icon className="h-3.5 w-3.5" />
                {k.label}
              </div>
              <p className="mt-1 truncate font-mono text-lg font-bold" title={k.value}>
                {k.value}
              </p>
              <p className="truncate text-[11px] text-muted-foreground">{k.sub}</p>
            </CardContent>
          </Card>
        ))}
      </div>

      <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
        <Card className="transition-all duration-200 hover:border-primary/30 hover:shadow-sm">
          <CardHeader className="pb-2">
            <div className="flex items-center gap-2">
              <CardTitle className="text-sm">What breaks down into what</CardTitle>
              {onGoTrace && (
                <Button size="sm" variant="ghost" className="ml-auto h-7 px-2 text-xs" onClick={onGoTrace}>
                  <Waypoints className="h-3.5 w-3.5" />
                  Interactive trace
                </Button>
              )}
            </div>
            <CardDescription>Query units by kind — each becomes a repository method.</CardDescription>
          </CardHeader>
          <CardContent>
            <Donut data={queryKinds} />
            {onGoTrace && (
              <p className="mt-2 text-[11px] text-muted-foreground">
                After converting, the Trace tab shows exactly which query landed in which file.
              </p>
            )}
          </CardContent>
        </Card>
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-sm">Contract traffic</CardTitle>
            <CardDescription>FML reads/writes — each scenario maps to an API.</CardDescription>
          </CardHeader>
          <CardContent>
            <Bars data={fmlKinds} label="FML op kinds" />
          </CardContent>
        </Card>
      </div>

      {coverage.total > 0 && (
        <Card>
          <CardContent className="flex items-center gap-4 p-4">
            <div className="flex-1">
              <div className="mb-1 flex items-center justify-between text-xs">
                <span className="font-medium">Flow classification</span>
                <span className="tabular-nums text-muted-foreground">
                  {coverage.cls}/{coverage.total} lines ({coverage.pct}%)
                </span>
              </div>
              <Progress value={coverage.pct} />
            </div>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
