// Pure, dependency-free helpers for the file-scoped rename + language detect.
// Kept separate from components/files.tsx so the logic is unit-testable
// without a React/Next runtime.

export type Lang = "go" | "py" | "cs" | "yaml" | "sql" | "c" | "plain";

export function langOf(path?: string): Lang {
  const p = (path ?? "").toLowerCase();
  if (/\.go$/.test(p)) return "go";
  if (/\.py$/.test(p)) return "py";
  if (/\.cs$/.test(p)) return "cs";
  if (/\.ya?ml$/.test(p)) return "yaml";
  if (/\.sql$/.test(p)) return "sql";
  if (/\.pc(f)?$|\.[ch]$/.test(p)) return "c";
  return "plain";
}

// File-scoped global rename: whole-word (default), case-sensitive, this file
// only. `from` is escaped — never interpreted as a regex.
export function applyRename(content: string, from: string, to: string, wholeWord: boolean): { next: string; count: number } {
  if (!from) return { next: content, count: 0 };
  const esc = from.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const pattern = wholeWord ? `\\b${esc}\\b` : esc;
  const re = new RegExp(pattern, "g");
  let count = 0;
  const next = content.replace(re, () => {
    count += 1;
    return to;
  });
  return { next, count };
}
