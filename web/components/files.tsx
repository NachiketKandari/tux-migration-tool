"use client";

import * as React from "react";
import { FileCode2, Folder, FolderOpen, Search, Pencil, Save, X, Copy, Check, WrapText, ArrowLeftRight } from "lucide-react";
import { cn } from "@/lib/utils";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Textarea } from "@/components/ui/textarea";
import { Skeleton } from "@/components/ui/skeleton";
import { applyRename, langOf, type Lang } from "@/lib/rename";

// ---------------------------------------------------------------------------
// FileTree — collapsible folders with per-folder counts + result counts.
// ---------------------------------------------------------------------------

export function FileTree({
  files,
  selected,
  onSelect,
}: {
  files: string[];
  selected: string | null;
  onSelect: (p: string) => void;
}) {
  const [q, setQ] = React.useState("");
  const [collapsed, setCollapsed] = React.useState<Set<string>>(new Set());
  const filtered = React.useMemo(() => {
    const needle = q.trim().toLowerCase();
    if (!needle) return files;
    return files.filter((f) => f.toLowerCase().includes(needle));
  }, [files, q]);

  const tree = React.useMemo(() => {
    const root: Record<string, unknown> = {};
    for (const f of filtered) {
      const parts = f.split("/");
      let node = root;
      for (let i = 0; i < parts.length; i++) {
        const last = i === parts.length - 1;
        if (last) (node as Record<string, string>)["\0" + parts[i]] = f;
        else {
          node[parts[i]] = node[parts[i]] ?? {};
          node = node[parts[i]] as Record<string, unknown>;
        }
      }
    }
    return root;
  }, [filtered]);

  function countFiles(node: Record<string, unknown>): number {
    let n = 0;
    for (const [k, v] of Object.entries(node)) {
      if (k.startsWith("\0")) n += 1;
      else n += countFiles(v as Record<string, unknown>);
    }
    return n;
  }

  function toggle(path: string) {
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(path)) next.delete(path);
      else next.add(path);
      return next;
    });
  }

  function render(node: Record<string, unknown>, depth: number, prefix: string): React.ReactNode {
    return Object.entries(node)
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([k, v]) => {
        if (k.startsWith("\0")) {
          const name = k.slice(1);
          const full = v as string;
          return (
            <button
              key={full}
              onClick={() => onSelect(full)}
              title={full}
              aria-current={selected === full}
              className={cn(
                "flex w-full items-center gap-1.5 rounded px-2 py-1 text-left font-mono text-xs hover:bg-accent",
                selected === full && "bg-accent font-semibold"
              )}
              style={{ paddingLeft: depth * 12 + 8 }}
            >
              <FileCode2 className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
              <span className="truncate">{name}</span>
            </button>
          );
        }
        const path = prefix ? `${prefix}/${k}` : k;
        const isCollapsed = collapsed.has(path);
        const n = countFiles(v as Record<string, unknown>);
        const FolderIcon = isCollapsed ? Folder : FolderOpen;
        return (
          <div key={path}>
            <button
              onClick={() => toggle(path)}
              aria-expanded={!isCollapsed}
              className="flex w-full items-center gap-1.5 rounded px-2 py-1 font-mono text-xs text-muted-foreground hover:bg-accent hover:text-foreground"
              style={{ paddingLeft: depth * 12 + 8 }}
              title={`${n} file${n === 1 ? "" : "s"}`}
            >
              <FolderIcon className="h-3.5 w-3.5 shrink-0" />
              <span className="truncate">{k}</span>
              <span className="ml-auto rounded-full bg-muted px-1.5 text-[10px] tabular-nums">{n}</span>
            </button>
            {!isCollapsed && render(v as Record<string, unknown>, depth + 1, path)}
          </div>
        );
      });
  }

  if (files.length === 0) return <p className="p-3 text-xs text-muted-foreground">No files yet — run a conversion.</p>;
  return (
    <div className="py-1">
      <div className="sticky top-0 flex items-center gap-2 bg-background/80 p-2 backdrop-blur">
        <div className="relative flex-1">
          <Search className="absolute left-2 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder={`Filter ${files.length} files…`}
            aria-label="Filter files"
            className="h-7 pl-7 text-xs"
          />
        </div>
        {q && (
          <Button variant="ghost" size="sm" className="h-7 px-2 text-xs" onClick={() => setQ("")}>
            Clear
          </Button>
        )}
      </div>
      <p className="px-3 pb-1 text-[11px] tabular-nums text-muted-foreground" aria-live="polite">
        {filtered.length} of {files.length} files
      </p>
      {filtered.length === 0 ? (
        <p className="p-3 text-xs text-muted-foreground">No files match “{q}”.</p>
      ) : (
        render(tree, 0, "")
      )}
    </div>
  );
}

export function FileTreeSkeleton() {
  return (
    <div className="space-y-2 p-3" aria-label="Loading files">
      <Skeleton className="h-7 w-full" />
      {[0, 1, 2, 3, 4].map((i) => (
        <div key={i} className="flex items-center gap-2">
          <Skeleton className="h-4 w-4" />
          <Skeleton className="h-4" style={{ width: `${60 - i * 7}%` }} />
        </div>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Lightweight syntax highlighting (no deps): line-based, per-extension.
// Colors use theme-aware tailwind tokens so dark mode stays readable.
// ---------------------------------------------------------------------------

// (Lang, langOf, applyRename live in @/lib/rename — re-exported here so
// existing imports from "@/components/files" keep working.)
export type { Lang };
export { applyRename, langOf } from "@/lib/rename";

const KEYWORDS: Record<Lang, string[]> = {
  go: ["func", "return", "if", "else", "for", "range", "switch", "case", "default", "break", "continue", "struct", "interface", "type", "map", "chan", "go", "defer", "package", "import", "var", "const", "nil", "true", "false", "err"],
  py: ["def", "return", "if", "elif", "else", "for", "while", "in", "import", "from", "as", "class", "pass", "break", "continue", "None", "True", "False", "with", "try", "except", "raise", "lambda"],
  cs: ["public", "private", "protected", "internal", "static", "class", "interface", "namespace", "using", "return", "if", "else", "for", "foreach", "while", "switch", "case", "default", "break", "continue", "var", "new", "null", "true", "false", "async", "await", "Task", "void", "string", "int"],
  yaml: [],
  sql: ["SELECT", "FROM", "WHERE", "INSERT", "INTO", "VALUES", "UPDATE", "SET", "DELETE", "MERGE", "JOIN", "LEFT", "RIGHT", "INNER", "OUTER", "ON", "AND", "OR", "NOT", "NULL", "AS", "ORDER", "BY", "GROUP", "HAVING", "UNION", "ALL", "DISTINCT"],
  c: ["if", "else", "for", "while", "do", "switch", "case", "default", "break", "continue", "return", "struct", "typedef", "int", "char", "long", "short", "double", "float", "void", "static", "const", "EXEC", "SQL"],
  plain: [],
};

function highlightTokens(line: string, lang: Lang, key: number): React.ReactNode[] {
  // Split keeping: comments, strings, numbers, words. Comments/strings win.
  const commentStart = lang === "yaml" ? /(#[^\n]*)/ : lang === "py" ? /(#[^\n]*)/ : /(\/\/[^\n]*)/;
  const m = line.match(commentStart);
  let code = line;
  let comment: string | null = null;
  if (m && m.index !== undefined) {
    // Don't treat # inside YAML quoted strings as comment — good enough: only
    // split when the # is at line start or preceded by whitespace.
    if (lang === "yaml" || lang === "py") {
      const i = m.index;
      if (i === 0 || /\s/.test(line[i - 1])) {
        code = line.slice(0, i);
        comment = line.slice(i);
      }
    } else {
      code = line.slice(0, m.index);
      comment = line.slice(m.index);
    }
  }
  const kws = KEYWORDS[lang];
  const kwSet = new Set(kws.concat(kws.map((w) => w.toLowerCase())));
  const parts = code.split(/('(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"|`(?:[^`\\]|\\.)*`|\b\d[\w.]*\b|\b[A-Za-z_][\w]*\b)/g);
  const out: React.ReactNode[] = [];
  parts.forEach((p, i) => {
    if (!p) return;
    const k = `${key}-${i}`;
    if (/^'[\s\S]*'$|^"[\s\S]*"$|^`[\s\S]*`$/.test(p)) {
      out.push(<span key={k} className="text-amber-700 dark:text-amber-300">{p}</span>);
    } else if (/^\d/.test(p)) {
      out.push(<span key={k} className="text-violet-700 dark:text-violet-300">{p}</span>);
    } else if (/^[A-Za-z_]/.test(p) && (kwSet.has(p) || (lang === "sql" && kwSet.has(p.toUpperCase())))) {
      out.push(<span key={k} className="font-semibold text-blue-700 dark:text-blue-300">{p}</span>);
    } else if (/^[A-Z][A-Za-z0-9_]*$/.test(p) && (lang === "go" || lang === "cs")) {
      out.push(<span key={k} className="text-teal-700 dark:text-teal-300">{p}</span>);
    } else {
      out.push(<span key={k}>{p}</span>);
    }
  });
  if (comment) out.push(<span key={`${key}-c`} className="italic text-muted-foreground">{comment}</span>);
  return out;
}

export function RenameBar({
  content,
  onApply,
  compact,
}: {
  content: string;
  onApply: (next: string, count: number) => void;
  compact?: boolean;
}) {
  const [from, setFrom] = React.useState("");
  const [to, setTo] = React.useState("");
  const [wholeWord, setWholeWord] = React.useState(true);
  const [note, setNote] = React.useState("");

  const matches = React.useMemo(() => {
    if (!from) return 0;
    return applyRename(content, from, to || from, wholeWord).count;
  }, [content, from, to, wholeWord]);

  function run() {
    if (!from.trim() || !to) {
      setNote("Enter both the current name and the new name.");
      return;
    }
    const { next, count } = applyRename(content, from.trim(), to, wholeWord);
    if (count === 0) {
      setNote(`No matches for “${from.trim()}” in this file.`);
      return;
    }
    onApply(next, count);
    setNote(`Renamed ${count} occurrence${count === 1 ? "" : "s"} in this file.`);
  }

  return (
    <div className={cn("flex flex-wrap items-center gap-1.5 rounded-md border bg-muted/40 p-1.5", compact && "text-xs")}>
      <ArrowLeftRight className="h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden />
      <Input
        value={from}
        onChange={(e) => {
          setFrom(e.target.value);
          setNote("");
        }}
        placeholder="Current name"
        aria-label="Current name to rename"
        className="h-7 w-32 font-mono text-xs"
      />
      <span className="text-xs text-muted-foreground">→</span>
      <Input
        value={to}
        onChange={(e) => {
          setTo(e.target.value);
          setNote("");
        }}
        placeholder="New name"
        aria-label="New name"
        className="h-7 w-32 font-mono text-xs"
        onKeyDown={(e) => {
          if (e.key === "Enter") run();
        }}
      />
      <label className="flex cursor-pointer items-center gap-1 text-[11px] text-muted-foreground">
        <input type="checkbox" checked={wholeWord} onChange={(e) => setWholeWord(e.target.checked)} className="accent-current" />
        whole-word
      </label>
      <Button size="sm" variant="secondary" className="h-7 px-2 text-xs" disabled={!from.trim() || !to} onClick={run}>
        Rename in file
      </Button>
      <span className="text-[11px] tabular-nums text-muted-foreground" aria-live="polite">
        {from ? `${matches} match${matches === 1 ? "" : "es"}` : note || "file-scoped"}
        {!from && note ? ` — ${note}` : from && note ? ` — ${note}` : ""}
      </span>
    </div>
  );
}

// ---------------------------------------------------------------------------
// CodeView — formatted view + in-place edit + file-scoped rename.
// ---------------------------------------------------------------------------

export function CodeView({
  content,
  path,
  editable,
  saving,
  onSave,
}: {
  content: string;
  path?: string;
  editable?: boolean;
  saving?: boolean;
  onSave?: (next: string) => void;
}) {
  const [copied, setCopied] = React.useState(false);
  const [editing, setEditing] = React.useState(false);
  const [draft, setDraft] = React.useState(content);
  const [override, setOverride] = React.useState<string | null>(null);
  const [wrap, setWrap] = React.useState(true);
  const [showRename, setShowRename] = React.useState(false);
  const lang = langOf(path);

  React.useEffect(() => {
    setDraft(content);
    setEditing(false);
    setOverride(null);
  }, [path, content]);

  // Local rename preview when the caller doesn't persist (Source, Scenarios):
  // shown swaps in the renamed text with a reset affordance.
  const shown = override ?? content;
  const lines = React.useMemo(() => shown.split("\n"), [shown]);
  const dirty = draft !== content;
  const draftLines = React.useMemo(() => draft.split("\n").length, [draft]);

  async function copy() {
    try {
      await navigator.clipboard.writeText(editing ? draft : shown);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      /* clipboard unavailable */
    }
  }

  function download() {
    const blob = new Blob([editing ? draft : shown], { type: "text/plain" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = (path ?? "file.txt").split("/").pop() ?? "file.txt";
    a.click();
    URL.revokeObjectURL(url);
  }

  return (
    <div>
      <div className="sticky top-0 flex flex-wrap items-center gap-1.5 border-b bg-background/90 p-2 backdrop-blur">
        <code className="min-w-0 flex-1 truncate font-mono text-[11px] text-muted-foreground" title={path ?? "preview"}>
          {path ?? "preview"} · {lines.length} lines · {lang}
          {override && " · renamed (preview)"}
        </code>
        <span className="flex items-center gap-1">
          {override && (
            <Button variant="ghost" size="sm" className="h-7 px-2 text-xs" onClick={() => setOverride(null)}>
              <X className="h-3.5 w-3.5" />
              Reset rename
            </Button>
          )}
          <Button
            variant="ghost"
            size="sm"
            className="h-7 px-2 text-xs"
            onClick={() => setWrap((w) => !w)}
            aria-pressed={wrap}
            title={wrap ? "Disable line wrap" : "Enable line wrap"}
          >
            <WrapText className="h-3.5 w-3.5" />
            {wrap ? "Wrap" : "No-wrap"}
          </Button>
          {!editing && (
            <Button variant="ghost" size="sm" className="h-7 px-2 text-xs" onClick={() => setShowRename((s) => !s)} aria-expanded={showRename}>
              <ArrowLeftRight className="h-3.5 w-3.5" />
              Rename
            </Button>
          )}
          {editable &&
            (editing ? (
              <>
                <Button
                  variant="ghost"
                  size="sm"
                  className="h-7 px-2 text-xs"
                  disabled={saving}
                  onClick={() => {
                    setDraft(content);
                    setEditing(false);
                  }}
                >
                  <X className="h-3.5 w-3.5" />
                  Discard
                </Button>
                <Button size="sm" className="h-7 px-2 text-xs" disabled={saving || !dirty} onClick={() => onSave?.(draft)}>
                  <Save className="h-3.5 w-3.5" />
                  {saving ? "Saving…" : "Save"}
                </Button>
              </>
            ) : (
              <Button variant="ghost" size="sm" className="h-7 px-2 text-xs" onClick={() => setEditing(true)}>
                <Pencil className="h-3.5 w-3.5" />
                Edit
              </Button>
            ))}
          <Button variant="ghost" size="sm" className="h-7 px-2 text-xs" onClick={copy}>
            {copied ? <Check className="h-3.5 w-3.5" /> : <Copy className="h-3.5 w-3.5" />}
            {copied ? "Copied" : "Copy"}
          </Button>
          <Button variant="ghost" size="sm" className="h-7 px-2 text-xs" onClick={download}>
            Download
          </Button>
        </span>
      </div>

      {showRename && !editing && (
        <div className="border-b p-2">
          <RenameBar
            content={shown}
            onApply={(next) => {
              // Persisted when the caller saves (Convert); otherwise a local
              // file-scoped preview with reset (Source, Scenarios).
              if (editable && onSave) onSave(next);
              else setOverride(next);
            }}
          />
          <p className="mt-1 text-[11px] text-muted-foreground">Rename is scoped to this file only — aliases, functions, structs.</p>
        </div>
      )}

      {editing ? (
        <div className="space-y-1.5 p-2">
          {editable && (
            <RenameBar
              content={draft}
              onApply={(next) => setDraft(next)}
            />
          )}
          <Textarea
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            spellCheck={false}
            rows={Math.min(30, Math.max(10, draftLines + 2))}
            aria-label={`Edit ${path ?? "file"}`}
            className="min-h-[320px] leading-5"
          />
          <div className="flex items-center gap-2 text-[11px] text-muted-foreground">
            {dirty ? <Badge variant="outline">edited</Badge> : <span>No changes.</span>}
            <span className="tabular-nums">
              {draftLines} lines · {draft.length} chars
            </span>
            {saving && <span>Saving…</span>}
          </div>
        </div>
      ) : (
        <pre className="overflow-auto p-3 font-mono text-xs leading-5">
          {lines.map((l, i) => (
            <div key={i} className="flex hover:bg-accent/40">
              <span className="w-10 shrink-0 select-none pr-3 text-right text-muted-foreground/60">{i + 1}</span>
              <code className={wrap ? "whitespace-pre-wrap break-all" : "whitespace-pre"}>{highlightTokens(l, lang, i)}</code>
            </div>
          ))}
        </pre>
      )}
    </div>
  );
}

export function CodeViewSkeleton({ lines = 12 }: { lines?: number }) {
  return (
    <div className="space-y-1.5 p-3" aria-label="Loading code">
      {[0, 1, 2].map((i) => (
        <Skeleton key={i} className="h-7 w-2/3" />
      ))}
      {Array.from({ length: lines }).map((_, i) => (
        <div key={i} className="flex gap-2">
          <Skeleton className="h-4 w-8 shrink-0" />
          <Skeleton className="h-4" style={{ width: `${85 - ((i * 13) % 50)}%` }} />
        </div>
      ))}
    </div>
  );
}
