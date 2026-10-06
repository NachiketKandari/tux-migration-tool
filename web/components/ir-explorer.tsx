"use client";

import * as React from "react";
import { Search } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { asArray, shortSql } from "@/lib/ir";

function FilteredTable({
  title,
  items,
  empty,
  hint,
}: {
  title: string;
  items: unknown[];
  empty: string;
  hint?: string;
}) {
  const [q, setQ] = React.useState("");
  const rows_raw = React.useMemo(
    () =>
      (items as unknown[]).map((o) =>
        o !== null && typeof o === "object" && !Array.isArray(o)
          ? (o as Record<string, unknown>)
          : typeof o === "string" || typeof o === "number" || typeof o === "boolean"
            ? { name: String(o) }
            : { value: JSON.stringify(o) }
      ),
    [items]
  );
  const cols = React.useMemo(() => {
    const keys: string[] = [];
    const seen = new Set<string>();
    for (const o of rows_raw.slice(0, 20)) {
      for (const [k, v] of Object.entries(o)) {
        if (seen.has(k)) continue;
        if (v === null || ["string", "number", "boolean"].includes(typeof v)) {
          seen.add(k);
          keys.push(k);
        }
        if (keys.length >= 6) break;
      }
      if (keys.length >= 6) break;
    }
    return keys.slice(0, 5);
  }, [rows_raw]);

  const rows = React.useMemo(() => {
    const needle = q.trim().toLowerCase();
    const base = rows_raw.slice(0, 200);
    if (!needle) return base;
    return base.filter((o) => JSON.stringify(o).toLowerCase().includes(needle));
  }, [rows_raw, q]);

  return (
    <Card>
      <CardHeader className="pb-2">
        <div className="flex items-center justify-between gap-2">
          <CardTitle className="text-sm">
            {title} <span className="font-normal text-muted-foreground">({items.length})</span>
          </CardTitle>
          <div className="relative w-48">
            <Search className="absolute left-2 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
            <Input value={q} onChange={(e) => setQ(e.target.value)} placeholder="Filter…" className="h-7 pl-7 text-xs" />
          </div>
        </div>
        {hint && <p className="text-[11px] text-muted-foreground">{hint}</p>}
      </CardHeader>
      <CardContent>
        {rows.length === 0 ? (
          <p className="py-4 text-center text-xs text-muted-foreground">{items.length === 0 ? empty : "No rows match."}</p>
        ) : cols.length === 0 ? (
          <pre className="whitespace-pre-wrap p-3 font-mono text-[11px] text-muted-foreground">
            {JSON.stringify(rows.slice(0, 10), null, 1).slice(0, 2000)}
          </pre>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                {cols.map((c) => (
                  <TableHead key={c} className="font-mono text-[11px]">
                    {c}
                  </TableHead>
                ))}
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.slice(0, 50).map((o, i) => (
                <TableRow key={i}>
                  {cols.map((c) => (
                    <TableCell key={c} className="max-w-[260px] truncate font-mono text-[11px]" title={String(o[c] ?? "")}>
                      {c.toLowerCase().includes("sql") ? shortSql(o[c]) : (o[c] ?? "—") === "" ? "—" : String(o[c] ?? "—")}
                    </TableCell>
                  ))}
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        {rows.length > 50 && (
          <p className="mt-2 text-[11px] text-muted-foreground">Showing 50 of {rows.length} — refine the filter.</p>
        )}
      </CardContent>
    </Card>
  );
}

export function IRExplorer({ ir }: { ir?: Record<string, unknown> }) {
  const conditions = asArray(ir, ["conditions", "Conditions"]);
  const queries = asArray(ir, ["queries", "Queries", "queryUnits", "QueryUnits"]);
  const functions = asArray(ir, ["functions", "Functions"]);
  const fmlOps = asArray(ir, ["fml_ops", "fmlOps", "fml", "FmlOps"]);
  const hostVars = asArray(ir, ["host_vars", "hostVars", "hostvars"]);

  if (!ir) return <p className="text-sm text-muted-foreground">Upload a file to inspect its IR.</p>;

  return (
    <div className="space-y-3">
      <FilteredTable title="Conditions" items={conditions} empty="No conditions extracted." hint="Branch predicates the flow tree folds per scenario." />
      <FilteredTable title="Query units" items={queries} empty="No query units extracted." hint="Each unit becomes one repository method; SQL fidelity is gated." />
      <FilteredTable title="Functions" items={functions} empty="No functions inventoried." />
      <FilteredTable title="FML ops" items={fmlOps} empty="No FML ops recorded." hint="Fget32 reads / Fadd32 writes — the API contract evidence." />
      <FilteredTable title="Host vars" items={hostVars} empty="No host vars recorded." />
    </div>
  );
}
