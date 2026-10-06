// Flow coercion — the tuxconv CLI emits snake_case (start_line, code_lines,
// classified_lines, ...) while older drafts used camelCase. Accept every known
// spelling so the viewer never shows "no coverage data" on a healthy report.
import type { FlowReport } from "@/lib/jobs";

function num(v: unknown): number | undefined {
  const n = Number(v);
  return Number.isFinite(n) ? n : undefined;
}

function pick(obj: Record<string, unknown> | undefined, ...keys: string[]): unknown {
  if (!obj) return undefined;
  for (const k of keys) {
    if (obj[k] !== undefined) return obj[k];
  }
  const lower = new Map(Object.keys(obj).map((k) => [k.toLowerCase(), k]));
  for (const k of keys) {
    const hit = lower.get(k.toLowerCase());
    if (hit && obj[hit] !== undefined) return obj[hit];
  }
  return undefined;
}

export function coerceFlowReport(raw: unknown, fallbackTarget: string): FlowReport | undefined {
  if (!raw || typeof raw !== "object") return undefined;
  const r = raw as { target?: string; files?: unknown };
  if (!Array.isArray(r.files)) return undefined;
  return {
    target: typeof r.target === "string" ? r.target : fallbackTarget,
    files: (r.files as Array<Record<string, unknown>>).map((f) => ({
      path: String(f.path ?? ""),
      functions: Array.isArray(f.functions)
        ? (f.functions as Array<Record<string, unknown>>).map((fn) => {
            const tree = (pick(fn, "Tree", "tree") ?? {}) as Record<string, unknown>;
            const cov = (pick(tree, "Coverage", "coverage") ?? {}) as Record<string, unknown>;
            const hints = pick(fn, "Hints", "hints") as Array<Record<string, unknown>> | undefined;
            const classified =
              num(pick(cov, "Classified", "classified", "classified_lines", "classifiedLines")) ?? 0;
            const codeLines =
              num(pick(cov, "CodeLines", "codeLines", "code_lines")) ?? 0;
            const unknown =
              num(pick(cov, "Unknown", "unknown", "unknown_lines", "unknownLines")) ?? 0;
            const residueRaw = pick(cov, "Residue", "residue", "residue_lines", "residueLines");
            const hasCoverage =
              cov && ("classified" in cov || "Classified" in cov || "classified_lines" in cov || "code_lines" in cov || "CodeLines" in cov || codeLines > 0 || classified > 0);
            return {
              name: String(pick(fn, "Name", "name") ?? "?"),
              startLine: num(pick(tree, "StartLine", "startLine", "start_line")) || undefined,
              endLine: num(pick(tree, "EndLine", "endLine", "end_line")) || undefined,
              coverage: hasCoverage
                ? {
                    classified,
                    codeLines,
                    unknown,
                    residue: Array.isArray(residueRaw)
                      ? (residueRaw as unknown[]).map(Number).filter(Number.isFinite).slice(0, 50)
                      : undefined,
                  }
                : undefined,
              hints: Array.isArray(hints)
                ? hints.slice(0, 100).map((h) => ({
                    kind: String(pick(h, "Kind", "kind") ?? "hint"),
                    line: Number(pick(h, "Line", "line") ?? 0),
                    detail: String(pick(h, "Detail", "detail") ?? ""),
                  }))
                : undefined,
              tree: (fn.Tree ?? fn.tree ?? undefined) as unknown,
            };
          })
        : [],
    })),
  };
}
