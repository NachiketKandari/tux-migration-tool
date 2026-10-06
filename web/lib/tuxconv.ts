import { spawn } from "node:child_process";
import { promises as fs } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { withServerConfig } from "@/lib/server-config";

// Repo root = parent of web/. tuxconv runs with cwd=job dir so every
// conversion_logs/ artifact stays inside the disposable job directory.
export const REPO_ROOT = join(process.cwd(), "..");

export function tuxBinary(): { cmd: string; preArgs: string[]; useShell: boolean } {
  if (process.env.TUXCONV_BIN) return { cmd: process.env.TUXCONV_BIN, preArgs: [], useShell: false };
  return { cmd: "go", preArgs: ["-C", REPO_ROOT, "run", "./cmd/tuxconv"], useShell: false };
}

export interface RunResult {
  code: number;
  stdout: string;
  stderr: string;
}

const MAX_OUT = 512 * 1024;

function trim(s: string): string {
  return s.length > MAX_OUT ? s.slice(0, MAX_OUT) + "\n…[truncated]" : s;
}

// runTux spawns the tuxconv CLI inside dir, always routing logs under
// dir/logs via the global -log-dir flag. -no-llm is appended by callers
// that want deterministic-only runs. When TUXCONV_CONFIG points at a server
// yaml, its -config is spliced after the subcommand (callers may still pass
// their own -config — explicit wins) so disposable job tmpdirs share the
// CLI's yaml keys instead of the stock defaults.
export function runTux(dir: string, args: string[], log: (line: string) => void): Promise<RunResult> {
  const { cmd, preArgs } = tuxBinary();
  const withCfg = withServerConfig(args);
  const full = [...preArgs, "-log-dir", join(dir, "logs"), ...withCfg];
  log(`$ ${cmd} ${full.join(" ")}`);
  return new Promise((resolve) => {
    const child = spawn(cmd, full, { cwd: dir, timeout: 5 * 60 * 1000 });
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (d: Buffer) => {
      const s = d.toString();
      stdout += s;
      for (const line of s.split("\n")) if (line.trim()) log("[out] " + line);
    });
    child.stderr.on("data", (d: Buffer) => {
      const s = d.toString();
      stderr += s;
      for (const line of s.split("\n")) if (line.trim()) log("[err] " + line);
    });
    child.on("error", (err) => resolve({ code: 1, stdout: trim(stdout), stderr: trim(stderr) + String(err) }));
    child.on("close", (code) => resolve({ code: code ?? 1, stdout: trim(stdout), stderr: trim(stderr) }));
  });
}

export async function newJobDir(): Promise<string> {
  return fs.mkdtemp(join(tmpdir(), "tux-web-"));
}

export async function listFilesRecursive(root: string): Promise<string[]> {
  const out: string[] = [];
  async function walk(dir: string) {
    const entries = await fs.readdir(dir, { withFileTypes: true });
    for (const e of entries) {
      const p = join(dir, e.name);
      if (e.isDirectory()) {
        if (e.name === "logs" || e.name === "node_modules") continue;
        await walk(p);
      } else {
        out.push(p);
      }
    }
  }
  try {
    await walk(root);
  } catch {
    return [];
  }
  out.sort();
  return out;
}
