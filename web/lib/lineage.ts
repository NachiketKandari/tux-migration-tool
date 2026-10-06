// Lineage — "this source part became that generated file".
//
// Pure module (no node imports) shared by the server route
// (app/api/jobs/[id]/lineage) and any client-side fallback. The mapping is
// evidence-based: every link carries the literal token that was found and a
// confidence level, so the UI can show *why* a file exists instead of
// guessing.
//
// Two evidence layers, strongest first:
//
//   ledger   — backend ground truth from conversion_logs/ledger/*.ledger.json
//              (source lines → generated file via the plan/ledger Map).
//              Links carry verified:true and confidence exact. The server
//              route loads these; the browser never parses them.
//   heuristic — token matching (tables, FML fields, tpcall services) against
//              file contents for everything the ledger does not cover
//              (Python/C# targets, scaffolding, unmapped arms).
//
// Source facts come from the IR snapshot (queries, conditions, fml_ops,
// tpcalls, functions). Targets are converted file contents read from the job
// tmpdir. Matching is deliberately conservative:
//   exact   — a source literal (table, FML field, tpcall service) appears
//             verbatim in the file, or the ledger Map names the file.
//   derived — a normalized form appears (USER_ID → user_id / UserId).
//   related — transitive ownership (a condition owns q1, q1 lands in db/…,
//             so the condition is related to db/…).
import { asArray, str } from "@/lib/ir";

export type SourceKind = "query" | "condition" | "fml" | "tpcall" | "function";
export type Confidence = "exact" | "derived" | "related";

export interface LineageTarget {
  file: string;
  layer: string;
  confidence: Confidence;
  reason: string;
  snippet?: string;
  /** True when the link comes from the backend ledger Map (source lines → file), not token matching. */
  verified?: boolean;
}

export interface LineageNode {
  id: string;
  kind: SourceKind;
  label: string;
  detail: string;
  meta: string;
  targets: LineageTarget[];
}

export interface LineageResult {
  nodes: LineageNode[];
  /** Every converted file, so the UI can also show "shared scaffolding". */
  files: string[];
  unmappedFiles: string[];
  generatedAt: string;
  /** Ledger Map entries consumed (backend ground truth). 0 = heuristic-only (Python/C#, pre-convert). */
  ledgerLinks?: number;
  /** Ledger files read (audit + live ledger dir). */
  ledgerFiles?: number;
}

export interface ConvertibleFile {
  path: string;
  content: string;
}

/** One backend ledger Map link: "SVC_X.pc :: Method (L61-66)" → generated file. */
export interface LedgerMap {
  source: string;
  target: string;
  name: string;
  start: number;
  end: number;
}

/** Parse a ledger Map source ("FILE :: Name (L61-66)" or "FILE :: Name"). */
export function parseLedgerSource(source: string): { name: string; start: number; end: number } | null {
  const m = source.match(/::\s*(.+?)\s*(?:\(L(\d+)(?:-(\d+))?\))?\s*$/);
  if (!m) return null;
  const name = (m[1] ?? "").trim();
  if (!name) return null;
  const start = m[2] ? Number(m[2]) : 0;
  const end = m[3] ? Number(m[3]) : start;
  return { name, start: Number.isFinite(start) ? start : 0, end: Number.isFinite(end) ? end : 0 };
}

function spansOverlap(aStart: number, aEnd: number, bStart: number, bEnd: number): boolean {
  if (!aStart || !bStart) return false;
  const aE = aEnd || aStart;
  const bE = bEnd || bStart;
  return aStart <= bE && bStart <= aE;
}

/** Resolve a ledger target ("svc/db/file.go") against converted files (suffix-tolerant). */
function resolveLedgerFile(target: string, files: IndexedFile[]): IndexedFile | null {
  const t = target.replace(/\\/g, "/");
  for (const f of files) {
    if (f.path === t) return f;
  }
  for (const f of files) {
    if (f.path.endsWith("/" + t) || t.endsWith("/" + f.path)) return f;
  }
  // Service-prefix tolerant: ledger "svc/db/x.go" vs converted "x.go" under a service root.
  const base = t.split("/").slice(-2).join("/");
  for (const f of files) {
    if (f.path.endsWith(base)) return f;
  }
  return null;
}

export function layerOf(path: string): string {
  const head = path.split("/")[0] ?? "";
  const known = new Set([
    "db",
    "controller",
    "handler",
    "models",
    "repository",
    "service",
    "dto",
    "controller_dto",
    "namedqueries",
    "repo",
  ]);
  const low = head.toLowerCase();
  if (known.has(low)) return low;
  if (/(^|\/)db(\/|$)/i.test(path)) return "db";
  if (/controller/i.test(path)) return "controller";
  if (/repositor/i.test(path)) return "repository";
  if (/quer/i.test(path)) return "namedqueries";
  if (/dto|model/i.test(path)) return "models";
  if (/handler|router/i.test(path)) return "handler";
  if (/\.py$/i.test(path)) return path.includes("repo") ? "repo" : "service";
  return "other";
}

function wordHit(hay: string, needle: string): number {
  if (!needle || needle.length < 2) return -1;
  // Word-ish match: escaped literal with non-word boundaries on both sides.
  const esc = needle.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const m = hay.match(new RegExp(`(^|[^A-Za-z0-9_])${esc}([^A-Za-z0-9_]|$)`, "i"));
  return m && m.index !== undefined ? m.index : -1;
}

function snippetAround(content: string, at: number, maxChars = 420): string {
  const lines = content.split("\n");
  let acc = 0;
  let li = 0;
  for (let i = 0; i < lines.length; i++) {
    acc += lines[i].length + 1;
    if (acc > at) {
      li = i;
      break;
    }
  }
  const slice = lines.slice(Math.max(0, li - 2), li + 3);
  const out = slice.join("\n").trim();
  return out.length > maxChars ? out.slice(0, maxChars - 1) + "…" : out;
}

function norm(s: string): string {
  return s.toLowerCase().replace(/[^a-z0-9]+/g, "_").replace(/^_+|_+$/g, "");
}

function camelOf(s: string): string {
  return s
    .toLowerCase()
    .split(/[^a-z0-9]+/)
    .filter(Boolean)
    .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
    .join("");
}

interface IndexedFile {
  path: string;
  layer: string;
  content: string;
  lower: string;
}

function findInFiles(
  idx: IndexedFile[],
  literals: string[],
  derivedTokens: string[],
  reasonFor: (token: string, file: string) => string,
  derivedReasonFor: (token: string, file: string) => string
): LineageTarget[] {
  const out: LineageTarget[] = [];
  const seen = new Set<string>();
  for (const f of idx) {
    let hit: LineageTarget | null = null;
    for (const lit of literals) {
      const at = lit ? wordHit(f.lower, lit.toLowerCase()) : -1;
      if (at >= 0) {
        hit = {
          file: f.path,
          layer: f.layer,
          confidence: "exact",
          reason: reasonFor(lit, f.path),
          snippet: snippetAround(f.content, at),
        };
        break;
      }
    }
    if (!hit) {
      for (const tok of derivedTokens) {
        const at = tok ? wordHit(f.lower, tok.toLowerCase()) : -1;
        if (at >= 0) {
          hit = {
            file: f.path,
            layer: f.layer,
            confidence: "derived",
            reason: derivedReasonFor(tok, f.path),
            snippet: snippetAround(f.content, at),
          };
          break;
        }
      }
    }
    if (hit && !seen.has(hit.file)) {
      seen.add(hit.file);
      out.push(hit);
    }
  }
  const rank: Record<Confidence, number> = { exact: 0, derived: 1, related: 2 };
  return out.sort((a, b) => rank[a.confidence] - rank[b.confidence] || a.file.localeCompare(b.file));
}

function strArr(v: unknown): string[] {
  if (Array.isArray(v)) return v.filter((x) => typeof x === "string") as string[];
  if (typeof v === "string") return [v];
  return [];
}

export function buildLineage(
  ir: Record<string, unknown> | undefined,
  files: ConvertibleFile[],
  ledgerMaps: LedgerMap[] = []
): LineageResult {
  const idx: IndexedFile[] = files.map((f) => ({
    path: f.path,
    layer: layerOf(f.path),
    content: f.content,
    lower: f.content.toLowerCase(),
  }));
  const nodes: LineageNode[] = [];
  let ledgerLinks = 0;

  // Ledger-verified targets for one source span: every Map entry whose
  // lines overlap the span, resolved against the converted tree. The
  // snippet anchors on the ledger method name so the evidence reads as
  // "this unit landed here" instead of a token guess.
  function ledgerTargetsFor(start: number, end: number, owner: string): LineageTarget[] {
    const out: LineageTarget[] = [];
    const seen = new Set<string>();
    for (const m of ledgerMaps) {
      if (!spansOverlap(start, end || start, m.start, m.end || m.start)) continue;
      const hit = resolveLedgerFile(m.target, idx);
      if (!hit || seen.has(hit.path)) continue;
      seen.add(hit.path);
      const at = wordHit(hit.lower, m.name.toLowerCase());
      out.push({
        file: hit.path,
        layer: hit.layer,
        confidence: "exact",
        reason: `ledger: ${m.name} → ${hit.path} (${owner})`,
        snippet: at >= 0 ? snippetAround(hit.content, at) : undefined,
        verified: true,
      });
    }
    return out.sort((a, b) => a.file.localeCompare(b.file));
  }

  function mergeVerifiedFirst(verified: LineageTarget[], heuristic: LineageTarget[]): LineageTarget[] {
    const seen = new Set(verified.map((t) => t.file));
    const rest = heuristic.filter((t) => !seen.has(t.file));
    ledgerLinks += verified.length;
    return [...verified, ...rest];
  }

  const queries = asArray(ir, ["queries", "Queries", "queryUnits", "QueryUnits"]);
  const conditions = asArray(ir, ["conditions", "Conditions"]);
  const fmlOps = asArray(ir, ["fml_ops", "fmlOps", "fml", "FmlOps"]);
  const tpcalls = asArray(ir, ["tpcalls", "Tpcalls", "tpCalls"]);
  const functions = asArray(ir, ["functions", "Functions"]);

  // Query id → targets, reused for condition "related" links.
  const queryTargets = new Map<string, LineageTarget[]>();

  queries.forEach((q, i) => {
    const id = str(q.id ?? q.ID ?? q.qid ?? q.name, `q${i + 1}`);
    const type = str(q.type ?? q.kind ?? q.Kind ?? q.op ?? q.queryKind, "QUERY");
    const sql = str(q.sql ?? q.SQL ?? q.text ?? q.statement, "");
    const tables = strArr(q.tables ?? q.Tables ?? q.table);
    const rowShape = strArr(q.row_shape ?? q.rowShape ?? q.RowShape);
    const binds = strArr(q.binds ?? q.Binds ?? q.bind_names);
    const sites = Array.isArray(q.sites) ? (q.sites as unknown[]).map(String).join(",") : "";
    const short = sql.replace(/\s+/g, " ").trim().slice(0, 140) || "(no SQL text)";

    // Literals: table names are the strongest signal — generated db methods
    // embed them verbatim in the query const. Also match the row-shape host
    // target when it is distinctive (≥4 chars).
    const literals = tables.filter((t) => t.length >= 2);
    const derivedTokens: string[] = [];
    for (const r of [...rowShape, ...binds]) {
      const n = norm(String(r));
      if (n.length >= 4) {
        derivedTokens.push(n, camelOf(String(r)));
      }
    }
    // INSERT INTO X / FROM X fallback when the tables array is empty.
    if (literals.length === 0 && sql) {
      const m = sql.match(/(?:from|into|update|merge\s+into)\s+([A-Za-z_][\w$]*)/i);
      if (m) literals.push(m[1]);
    }

    const targets = findInFiles(
      idx,
      literals,
      derivedTokens,
      (tok) => `table ${tok} in file`,
      (tok) => `row/bind ${tok} in file`
    );
    // Ledger first: the backend Map joins on source lines (q1 L61-66 →
    // GetMinAccounts → db/…). Heuristic stays as fallback.
    const qStart = Number(q.start_line ?? q.startLine ?? 0) || 0;
    const qEnd = Number(q.end_line ?? q.endLine ?? 0) || qStart;
    const verified = qStart ? ledgerTargetsFor(qStart, qEnd, id) : [];
    const merged = mergeVerifiedFirst(verified, targets);
    queryTargets.set(id.toLowerCase(), merged);
    queryTargets.set(id, merged);
    nodes.push({
      id,
      kind: "query",
      label: `${id} · ${type}`,
      detail: short,
      meta: [sites ? `L${sites}` : "", tables.length ? tables.join(", ") : ""].filter(Boolean).join(" · "),
      targets: merged,
    });
  });

  conditions.forEach((c, i) => {
    const n = Number(c.index ?? c.Index ?? i + 1);
    const id = `c${Number.isFinite(n) ? n : i + 1}`;
    const kind = str(c.kind ?? c.Kind, "branch");
    const expr = str(c.expr ?? c.Expr ?? c.predicateText ?? c.text, kind === "else" ? "else (default arm)" : kind);
    const start = Number(c.start_line ?? c.startLine ?? 0) || 0;
    const end = Number(c.end_line ?? c.endLine ?? 0) || 0;
    const qids = strArr(c.query_ids ?? c.queryIds ?? c.QueryIds ?? c.queries);
    const rel: LineageTarget[] = [];
    const seen = new Set<string>();
    for (const qid of qids) {
      for (const t of queryTargets.get(qid) ?? queryTargets.get(String(qid).toLowerCase()) ?? []) {
        if (seen.has(t.file)) continue;
        seen.add(t.file);
        rel.push({ ...t, confidence: "related", reason: `via ${qid} — arm owns this query` });
      }
    }
    // Condition-owned FML fields also pull their files in as related.
    const ownedFml = asArray(c as Record<string, unknown>, ["fml_ops", "fmlOps", "fml"]);
    for (const f of ownedFml) {
      const field = str(f.field ?? f.Field ?? f.name, "");
      if (!field) continue;
      for (const file of idx) {
        if (seen.has(file.path)) continue;
        if (wordHit(file.lower, field.toLowerCase()) >= 0 || wordHit(file.lower, norm(field)) >= 0) {
          seen.add(file.path);
          rel.push({
            file: file.path,
            layer: file.layer,
            confidence: "related",
            reason: `via ${field} — arm touches this buffer field`,
          });
        }
      }
    }
    const rank: Record<Confidence, number> = { exact: 0, derived: 1, related: 2 };
    // Ledger-verified arm links first (c1 L51-73 → GetAccId → controller/…),
    // then the transitive query/FML ownership as fallback. Verified links
    // keep confidence exact so the UI can badge them.
    const verifiedArm = start ? ledgerTargetsFor(start, end || start, id) : [];
    for (const t of verifiedArm) {
      if (!seen.has(t.file)) {
        seen.add(t.file);
        rel.push(t);
      }
    }
    rel.sort((a, b) => {
      const av = a.verified ? 0 : 1;
      const bv = b.verified ? 0 : 1;
      if (av !== bv) return av - bv;
      return rank[a.confidence] - rank[b.confidence] || a.file.localeCompare(b.file);
    });
    ledgerLinks += verifiedArm.length;
    nodes.push({
      id,
      kind: "condition",
      label: `${id} · ${expr.length > 42 ? expr.slice(0, 41) + "…" : expr}`,
      detail: expr,
      meta: [start && end ? `L${start}–${end}` : start ? `L${start}` : "", qids.length ? `owns ${qids.join(", ")}` : "no queries"]
        .filter(Boolean)
        .join(" · "),
      targets: rel.slice(0, 12),
    });
  });

  // FML ops can repeat per line — fold by field+kind so the trace stays
  // readable on big services, keeping the line list as evidence.
  const fmlGroups = new Map<string, { field: string; kind: string; lines: number[]; target?: string }>();
  fmlOps.forEach((f) => {
    const field = str(f.field ?? f.Field ?? f.name, "");
    if (!field) return;
    const kind = str(f.kind ?? f.Kind ?? f.op, "fml").toLowerCase();
    const key = `${field}::${kind}`;
    const line = Number(f.line ?? f.Line ?? 0) || 0;
    const g = fmlGroups.get(key) ?? { field, kind, lines: [] };
    if (line) g.lines.push(line);
    const tgt = str(f.target ?? f.Target, "");
    if (tgt && !g.target) g.target = tgt;
    fmlGroups.set(key, g);
  });
  // Also fold condition-embedded fml_ops (they carry the arm context).
  conditions.forEach((c) => {
    for (const f of asArray(c as Record<string, unknown>, ["fml_ops", "fmlOps", "fml"])) {
      const field = str(f.field ?? f.Field ?? f.name, "");
      if (!field || fmlGroups.has(`${field}::${str(f.kind ?? f.Kind ?? f.op, "fml").toLowerCase()}`)) continue;
      const kind = str(f.kind ?? f.Kind ?? f.op, "fml").toLowerCase();
      const line = Number(f.line ?? f.Line ?? 0) || 0;
      fmlGroups.set(`${field}::${kind}`, { field, kind, lines: line ? [line] : [] });
    }
  });

  for (const g of Array.from(fmlGroups.values()).slice(0, 120)) {
    const stripped = g.field.replace(/^FML_/i, "");
    const literals = [g.field];
    const derivedTokens = [stripped, norm(stripped), norm(g.field), camelOf(stripped)].filter(
      (t, i, a) => t.length >= 3 && a.indexOf(t) === i
    );
    const targets = findInFiles(
      idx,
      literals,
      derivedTokens,
      (tok) => `${tok} literal in file`,
      (tok) => `${g.field} → ${tok} in file`
    );
    const lines = Array.from(new Set(g.lines)).sort((a, b) => a - b).slice(0, 8);
    nodes.push({
      id: `fml:${g.field}:${g.kind}`,
      kind: "fml",
      label: `${g.field} · ${g.kind}`,
      detail: g.target ? `→ ${g.target}` : g.kind === "get" ? "read into host var" : "write to buffer",
      meta: lines.length ? `L${lines.join(", L")}` : "",
      targets: targets.slice(0, 12),
    });
  }

  tpcalls.forEach((t, i) => {
    const svc = str(t.service ?? t.Service ?? t.name, `tpcall${i + 1}`);
    const heuristic = findInFiles(idx, [svc], [norm(svc)], () => `tpcall ${svc} recorded`, (tok) => `tpcall ${svc} → ${tok}`);
    // Ledger tpcall placeholders (L79-83 → tpcall_placeholders.go) verify the site.
    const line = Number(
      (t as Record<string, unknown>).start_line ??
        (t as Record<string, unknown>).startLine ??
        (t as Record<string, unknown>).line ??
        0
    );
    const verified = line ? ledgerTargetsFor(line, line, `tpcall:${svc}`) : [];
    const targets = mergeVerifiedFirst(verified, heuristic);
    nodes.push({
      id: `tpcall:${svc}`,
      kind: "tpcall",
      label: `tpcall ${svc}`,
      detail: `tpcall to ${svc}`,
      meta: "",
      targets: targets.slice(0, 12),
    });
  });

  functions.forEach((f, i) => {
    const name = typeof f === "string" ? f : str((f as Record<string, unknown>).name ?? (f as Record<string, unknown>).Name, `fn${i + 1}`);
    if (!name || name === `fn${i + 1}`) return;
    const n = norm(name);
    const rel: LineageTarget[] = [];
    for (const file of idx) {
      if (file.lower.includes(n) || file.path.toLowerCase().includes(n)) {
        rel.push({
          file: file.path,
          layer: file.layer,
          confidence: "related",
          reason: `${name} tree rooted here`,
          snippet: undefined,
        });
      }
      if (rel.length >= 12) break;
    }
    // Functions are structural context — keep them last and collapsible.
    nodes.push({ id: `fn:${name}`, kind: "function", label: name, detail: "inventoried function", meta: "", targets: rel });
  });

  const order: Record<SourceKind, number> = { condition: 0, query: 1, fml: 2, tpcall: 3, function: 4 };
  nodes.sort((a, b) => order[a.kind] - order[b.kind] || a.id.localeCompare(b.id));

  // Functions are structural context (every path carries the service stem),
  // so they must not mark files as "owned" — otherwise nothing reads as
  // shared scaffolding. Only condition/query/fml/tpcall links confer
  // ownership.
  const mapped = new Set<string>();
  for (const n of nodes) {
    if (n.kind === "function") continue;
    for (const t of n.targets) mapped.add(t.file);
  }
  return {
    nodes,
    files: idx.map((f) => f.path),
    unmappedFiles: idx.map((f) => f.path).filter((p) => !mapped.has(p)),
    generatedAt: new Date().toISOString(),
    ledgerLinks,
    ledgerFiles: ledgerMaps.length ? new Set(ledgerMaps.map((m) => m.target)).size : 0,
  };
}

/** Targets pointing at one file — the "why does this file exist" banner. */
export function provenanceForFile(
  res: LineageResult | null,
  file: string
): { node: string; kind: SourceKind; reason: string; confidence: Confidence; verified?: boolean }[] {
  if (!res) return [];
  const out: { node: string; kind: SourceKind; reason: string; confidence: Confidence; verified?: boolean }[] = [];
  for (const n of res.nodes) {
    for (const t of n.targets) {
      if (t.file === file)
        out.push({ node: `${n.id} · ${n.label}`, kind: n.kind, reason: t.reason, confidence: t.confidence, verified: t.verified });
    }
  }
  const rank: Record<Confidence, number> = { exact: 0, derived: 1, related: 2 };
  return out
    .sort((a, b) => (a.verified === b.verified ? rank[a.confidence] - rank[b.confidence] : a.verified ? -1 : 1))
    .slice(0, 8);
}
