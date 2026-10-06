"use client";

import * as React from "react";
import { Gauge, Loader2, Play, Download, Search } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import type { AnalysisRow } from "@/lib/jobs";

function tierVariant(t: string): "default" | "secondary" | "destructive" | "outline" {
  const u = t.toUpperCase();
  if (u === "HIGH") return "destructive";
  if (u === "MEDIUM") return "default";
  if (u === "LOW") return "secondary";
  return "outline";
}

export function AnalyzePanel({
  jobId,
  analysis,
  csv,
  busy,
  onRun,
}: {
  jobId: string;
  analysis?: AnalysisRow[];
  csv?: string;
  busy: boolean;
  onRun: () => void;
}) {
  const [q, setQ] = React.useState("");
  const rows = React.useMemo(() => {
    const list = analysis ?? [];
    const needle = q.trim().toLowerCase();
    if (!needle) return list;
    return list.filter((r) => `${r.file} ${r.complexity} ${r.reasons}`.toLowerCase().includes(needle));
  }, [analysis, q]);

  const totals = React.useMemo(() => {
    const list = analysis ?? [];
    return {
      files: list.length,
      queries: list.reduce((a, r) => a + r.numQueries, 0),
      high: list.filter((r) => r.complexity.toUpperCase() === "HIGH").length,
      medium: list.filter((r) => r.complexity.toUpperCase() === "MEDIUM").length,
    };
  }, [analysis]);

  function downloadCsv() {
    if (!csv) return;
    const blob = new Blob([csv], { type: "text/csv" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = "triage_report.csv";
    a.click();
    URL.revokeObjectURL(url);
  }

  if (!analysis || analysis.length === 0) {
    return (
      <Card>
        <CardHeader className="pb-2">
          <CardTitle className="flex items-center gap-2 text-sm">
            <Gauge className="h-4 w-4" />
            Analyze — triage &amp; effort estimate
          </CardTitle>
          <CardDescription>
            Complexity rubric over the uploaded sources (queries, branching, external fns, tpcalls → LOW /
            MEDIUM / HIGH). Deterministic — runs the same <code className="font-mono">tuxconv analyze</code> as the
            CLI, no LLM calls.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <Button size="sm" disabled={busy} onClick={onRun}>
            {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Play className="h-3.5 w-3.5" />}
            Run analysis
          </Button>
        </CardContent>
      </Card>
    );
  }

  return (
    <div className="space-y-3">
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        {[
          { label: "Files", value: String(totals.files) },
          { label: "Query units", value: String(totals.queries) },
          { label: "HIGH", value: String(totals.high) },
          { label: "MEDIUM", value: String(totals.medium) },
        ].map((k) => (
          <Card key={k.label}>
            <CardContent className="p-4">
              <p className="text-[11px] font-medium uppercase tracking-wide text-muted-foreground">{k.label}</p>
              <p className="mt-1 font-mono text-lg font-bold">{k.value}</p>
            </CardContent>
          </Card>
        ))}
      </div>
      <Card>
        <CardHeader className="pb-2">
          <div className="flex flex-wrap items-center gap-2">
            <CardTitle className="flex items-center gap-2 text-sm">
              <Gauge className="h-4 w-4" />
              Triage report
            </CardTitle>
            <Badge variant="secondary" className="tabular-nums">
              {rows.length} of {analysis.length}
            </Badge>
            <span className="ml-auto flex gap-1.5">
              <Button size="sm" variant="ghost" disabled={busy} onClick={onRun}>
                {busy ? <Loader2 className="h-3.5 w-3.5 animate-spin" /> : <Play className="h-3.5 w-3.5" />}
                Re-run
              </Button>
              {csv && (
                <Button size="sm" variant="outline" onClick={downloadCsv}>
                  <Download className="h-3.5 w-3.5" />
                  triage_report.csv
                </Button>
              )}
            </span>
          </div>
          <CardDescription>
            Score = queries + branching + external-fn weights + tpcalls. HIGH first when planning conversion
            order. Full rubric in <code className="font-mono">tuxconv analyze</code>.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <div className="relative mb-2 w-64">
            <Search className="absolute left-2 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
            <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="Filter files…" className="h-7 pl-7 text-xs" />
          </div>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="font-mono text-[11px]">file</TableHead>
                <TableHead className="text-right font-mono text-[11px]">lines</TableHead>
                <TableHead className="text-right font-mono text-[11px]">queries</TableHead>
                <TableHead className="text-right font-mono text-[11px]">branch</TableHead>
                <TableHead className="text-right font-mono text-[11px]">ext fns</TableHead>
                <TableHead className="text-right font-mono text-[11px]">score</TableHead>
                <TableHead className="font-mono text-[11px]">tier</TableHead>
                <TableHead className="font-mono text-[11px]">why</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((r) => (
                <TableRow key={r.file}>
                  <TableCell className="max-w-[220px] truncate font-mono text-[11px]" title={r.file}>
                    {r.file}
                  </TableCell>
                  <TableCell className="text-right tabular-nums text-[11px]">{r.numLines}</TableCell>
                  <TableCell className="text-right tabular-nums text-[11px]">{r.numQueries}</TableCell>
                  <TableCell className="text-right tabular-nums text-[11px]" title={`${r.branchCount} headers`}>
                    {r.branchingFactor}
                  </TableCell>
                  <TableCell className="text-right tabular-nums text-[11px]">{r.fnExternalCount}</TableCell>
                  <TableCell className="text-right tabular-nums text-[11px]">{r.complexityScore}</TableCell>
                  <TableCell>
                    <Badge variant={tierVariant(r.complexity)} className="font-mono text-[10px]">
                      {r.complexity}
                    </Badge>
                  </TableCell>
                  <TableCell className="max-w-[320px] truncate text-[11px] text-muted-foreground" title={r.reasons}>
                    {r.reasons || "—"}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          {rows.length === 0 && <p className="py-4 text-center text-xs text-muted-foreground">No rows match.</p>}
          <p className="mt-2 hidden">{jobId}</p>
        </CardContent>
      </Card>
    </div>
  );
}
