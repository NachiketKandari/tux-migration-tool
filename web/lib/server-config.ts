import { statSync } from "node:fs";
import { readFileSync } from "node:fs";
import { isAbsolute, join } from "node:path";

// Server-level run config (TUXCONV_CONFIG) — the web viewer's answer to the
// disposable-tmpdir problem. TUXCONVERT_CONFIG / TUXGO_CONFIG are honored as
// fallbacks (in that order) so pre-sanitization server environments keep
// working.
//
// Every tuxconv call the viewer spawns runs with cwd=<job tmpdir>, and job
// tmpdirs carry no .tuxconv.yaml — so without this, browser runs always fell
// back to the stock defaults (profile `onprem-vllm`, key = $VLLM_API_KEY)
// while the CLI next door read yaml literals from the working copy. Set
// TUXCONV_CONFIG once for the server process:
//
//   export TUXCONV_CONFIG=$PWD/.tuxconv.yaml   # any path; literals stay gitignored
//
// and every viewer run shares the CLI's yaml keys (models[].apiKey,
// database.dsn — env vars still win over literals, same resolution).
// When no pointer (TUXCONV_CONFIG or a legacy spelling) resolves,
// serverConfigPath falls back to the
// working-copy .tuxconv.yaml at the repo root (the CLI's `./.tuxconv.yaml`
// discovery), so the plain README flow works without an export;
// an explicit-but-missing TUXCONV_CONFIG still resolves to null (loud
// warning, stock defaults) so typos never mask as another file.
//
// Two layers, both fail-open to today's behavior:
//
//  1. runTux injects `-config <abs path>` into every supporting subcommand
//     (explicit caller -config still wins — injection skips when present).
//  2. The tuxconv CLI itself falls back to $TUXCONV_CONFIG when neither
//     -config nor ./.tuxconv.yaml resolves (cmd/tuxconv/extract.go), so
//     direct CLI use from a bare directory honors it too.
//
// Nothing here ever returns a secret value — only presence booleans and
// the profile name. Key/DSN values stay in the server process and the
// spawned CLI; the browser sees booleans via /api/llm-status + /api/db-status.

const ENV_VAR = "TUXCONV_CONFIG";

// Legacy pre-sanitization pointers, honored in order after ENV_VAR.
const LEGACY_ENV_VARS = ["TUXCONVERT_CONFIG", "TUXGO_CONFIG"];

// Working-copy names probed when no pointer resolves: the primary spelling
// first, then the legacy pre-sanitization ones.
const WORKING_COPY_FILES = [".tuxconv.yaml", ".tuxconvert.yaml", ".tuxgo.yaml"];

// Subcommands accepting -config (mirrors cmd/tuxconv/flags.go commandFlags).
// `analyze` takes none, so injection skips it; version/help/retrystats take
// none either and the viewer never spawns them.
const CONFIG_COMMANDS = new Set([
  "extract",
  "plan",
  "convertgo",
  "discover",
  "convertbatchpy",
  "convertcs",
  "gentest",
  "ainames",
  "templates",
  "flow",
  "dbcheck",
]);

// Repo root = parent of web/. Absolute -config paths survive the per-job
// cwd (job tmpdirs); a relative TUXCONV_CONFIG resolves against the repo root
// first, then the server cwd.
function repoRoot(): string {
  return join(process.cwd(), "..");
}

let warnedMissing = "";

export function serverConfigPath(): string | null {
  let raw = "";
  let which = "";
  for (const name of [ENV_VAR, ...LEGACY_ENV_VARS]) {
    const v = (process.env[name] ?? "").trim();
    if (v !== "") {
      raw = v;
      which = name;
      break;
    }
  }
  if (raw !== "") {
    const candidates = isAbsolute(raw) ? [raw] : [join(repoRoot(), raw), join(process.cwd(), raw)];
    for (const c of candidates) {
      try {
        if (statSync(c).isFile()) {
          warnedMissing = "";
          return c;
        }
      } catch {
        /* try next */
      }
    }
    if (warnedMissing !== raw) {
      warnedMissing = raw;
      console.warn(`[tux-web] ${which}=${raw} names no file — running on stock defaults`);
    }
    return null;
  }
  // No pointer: mirror the CLI's `./.tuxconv.yaml` discovery so a working-copy
  // yaml (the README's `cp configs/.tuxconv.example.yaml .tuxconv.yaml` flow)
  // lights the header without forcing every operator to export TUXCONV_CONFIG.
  // An explicit-but-missing pointer above stays loud (null, no silent
  // fallback) so a typo'd pointer never masks as another file. The legacy
  // pre-sanitization spellings resolve here too.
  for (const name of WORKING_COPY_FILES) {
    for (const c of [join(repoRoot(), name), join(process.cwd(), name)]) {
      try {
        if (statSync(c).isFile()) {
          return c;
        }
      } catch {
        /* try next */
      }
    }
  }
  return null;
}

// configArgsFor returns ["-config", path] when the subcommand supports the
// flag, the caller did not pass one already, and TUXCONV_CONFIG resolves.
export function configArgsFor(subcommand: string, args: string[]): string[] {
  if (!CONFIG_COMMANDS.has(subcommand)) return [];
  if (args.some((a) => a === "-config" || a === "--config" || a.startsWith("-config=") || a.startsWith("--config="))) {
    return [];
  }
  const p = serverConfigPath();
  return p ? ["-config", p] : [];
}

// withServerConfig splices the server -config right after the subcommand
// (args[0]), so `runTux(dir, ["convertgo", src, ...])` keeps reading
// naturally at every call site.
export function withServerConfig(args: string[]): string[] {
  if (args.length === 0) return args;
  const extra = configArgsFor(args[0], args.slice(1));
  if (extra.length === 0) return args;
  return [args[0], ...extra, ...args.slice(1)];
}

export interface ServerModelSnapshot {
  name: string;
  apiKeyEnv: string;
  hasLiteralApiKey: boolean;
}

export interface ServerConfigSnapshot {
  path: string;
  profile: string;
  models: ServerModelSnapshot[];
  database: { dsnEnv: string; hasLiteralDsn: boolean };
}

function unquote(v: string): string {
  const t = v.trim();
  if (t.length >= 2 && ((t.startsWith('"') && t.endsWith('"')) || (t.startsWith("'") && t.endsWith("'")))) {
    return t.slice(1, -1).trim();
  }
  return t;
}

// stripComment cuts a trailing `# comment` — only when the hash starts the
// line or follows whitespace, so `sk-or#key`-style values survive.
function stripComment(line: string): string {
  let inSingle = false;
  let inDouble = false;
  for (let i = 0; i < line.length; i++) {
    const c = line[i];
    if (c === "'" && !inDouble) inSingle = !inSingle;
    else if (c === '"' && !inSingle) inDouble = !inDouble;
    else if (c === "#" && !inSingle && !inDouble && (i === 0 || /\s/.test(line[i - 1]))) {
      return line.slice(0, i);
    }
  }
  return line;
}

// readServerConfigSnapshot parses just the key-resolution surface of the
// server yaml (profile name, per-model apiKeyEnv + literal presence,
// database dsnEnv + literal presence). Values are never returned — only
// whether a literal is set. Unknown lines are ignored; a missing or
// unreadable file yields null (callers fall back to stock-profile checks).
export function readServerConfigSnapshot(): ServerConfigSnapshot | null {
  const path = serverConfigPath();
  if (!path) return null;
  let text: string;
  try {
    text = readFileSync(path, "utf8");
  } catch {
    return null;
  }
  const snap: ServerConfigSnapshot = {
    path,
    profile: "",
    models: [],
    database: { dsnEnv: "", hasLiteralDsn: false },
  };
  let section = "";
  let current: ServerModelSnapshot | null = null;
  for (const rawLine of text.split("\n")) {
    const line = stripComment(rawLine);
    if (line.trim() === "") continue;
    const indent = line.length - line.trimStart().length;
    const t = line.trim();
    if (indent === 0 && t.endsWith(":")) {
      section = t.slice(0, -1);
      current = null;
      continue;
    }
    if (section === "run" && indent > 0) {
      const m = t.match(/^profile\s*:\s*(.+)$/);
      if (m) snap.profile = unquote(m[1]);
      continue;
    }
    if (section === "models") {
      const start = t.match(/^-\s*name\s*:\s*(.+)$/);
      if (start) {
        current = { name: unquote(start[1]), apiKeyEnv: "", hasLiteralApiKey: false };
        snap.models.push(current);
        continue;
      }
      if (current && indent > 0) {
        let m = t.match(/^apiKeyEnv\s*:\s*(.*)$/);
        if (m) {
          current.apiKeyEnv = unquote(m[1]);
          continue;
        }
        m = t.match(/^apiKey\s*:\s*(.*)$/);
        if (m) {
          current.hasLiteralApiKey = unquote(m[1]) !== "";
          continue;
        }
      }
      continue;
    }
    if (section === "database" && indent > 0) {
      let m = t.match(/^dsnEnv\s*:\s*(.*)$/);
      if (m) {
        snap.database.dsnEnv = unquote(m[1]);
        continue;
      }
      m = t.match(/^dsn\s*:\s*(.*)$/);
      if (m) {
        snap.database.hasLiteralDsn = unquote(m[1]) !== "";
      }
    }
  }
  return snap;
}

// modelResolves mirrors internal/config Model.ResolveKey without touching
// values: env var wins, yaml literal is the fallback.
export function modelResolves(m: ServerModelSnapshot): boolean {
  if (m.apiKeyEnv !== "" && (process.env[m.apiKeyEnv] ?? "").trim() !== "") return true;
  return m.hasLiteralApiKey;
}

// dsnResolves mirrors internal/db ResolveDSN presence: env wins, literal fallback.
export function dsnResolves(dsnEnv: string, hasLiteral: boolean): boolean {
  const env = (dsnEnv || "ORACLE_DSN").trim() || "ORACLE_DSN";
  if ((process.env[env] ?? "").trim() !== "") return true;
  return hasLiteral;
}
