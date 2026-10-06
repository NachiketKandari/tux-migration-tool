import { promises as fs } from "node:fs";
import { join, relative, basename } from "node:path";
import { NextResponse } from "next/server";
import { getJob } from "@/lib/jobs";

// GET /api/jobs/:id/archive — download the whole converted tree as a .zip.
//
// Layout inside the zip:
//   <target>/<converted files…>   — the generated tree (root-relative paths)
//   mapping/<draft>.yaml           — the reviewed mapping, when one is saved
//   tux-migration-tool-summary.txt         — target, files, effective LLM mode, job name
//
// Converted files live on the server under a disposable tmpdir
// (<tmp>/tux-web-*/go|py|cs); jobs vanish on server restart, so this
// endpoint is the durable copy. Pure-Node STORE zip (no compression, no
// new deps) — works on alpine without a `zip` binary.

function crc32(buf: Buffer): number {
  let table = (crc32 as { t?: Uint32Array }).t;
  if (!table) {
    table = new Uint32Array(256);
    for (let n = 0; n < 256; n++) {
      let c = n;
      for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
      table[n] = c >>> 0;
    }
    (crc32 as { t?: Uint32Array }).t = table;
  }
  let crc = 0xffffffff;
  for (let i = 0; i < buf.length; i++) crc = table![(crc ^ buf[i]) & 0xff] ^ (crc >>> 8);
  return (crc ^ 0xffffffff) >>> 0;
}

function dosTime(d: Date): { time: number; date: number } {
  const time = ((d.getHours() & 31) << 11) | ((d.getMinutes() & 63) << 5) | ((Math.floor(d.getSeconds() / 2)) & 31);
  const date = (((d.getFullYear() - 1980) & 127) << 9) | (((d.getMonth() + 1) & 15) << 5) | (d.getDate() & 31);
  return { time, date };
}

// Minimal ZIP writer (STORE = method 0, UTF-8 flag). Entries must use
// forward slashes and must not escape (no leading /, no ".." segments).
// NOTE: not exported — Next.js route files may only export HTTP handlers.
function buildZip(entries: { name: string; data: Buffer }[]): Buffer {
  const now = dosTime(new Date());
  const parts: Buffer[] = [];
  const central: Buffer[] = [];
  let offset = 0;
  for (const e of entries) {
    const name = Buffer.from(e.name, "utf8");
    const crc = crc32(e.data);
    const size = e.data.length;
    const local = Buffer.alloc(30);
    local.writeUInt32LE(0x04034b50, 0);
    local.writeUInt16LE(20, 4);
    local.writeUInt16LE(0x0800, 6); // UTF-8 filenames
    local.writeUInt16LE(0, 8); // STORE
    local.writeUInt16LE(now.time, 10);
    local.writeUInt16LE(now.date, 12);
    local.writeUInt32LE(crc, 14);
    local.writeUInt32LE(size, 18);
    local.writeUInt32LE(size, 22);
    local.writeUInt16LE(name.length, 26);
    local.writeUInt16LE(0, 28);
    parts.push(local, name, e.data);

    const cen = Buffer.alloc(46);
    cen.writeUInt32LE(0x02014b50, 0);
    cen.writeUInt16LE(20, 4);
    cen.writeUInt16LE(20, 6);
    cen.writeUInt16LE(0x0800, 8);
    cen.writeUInt16LE(0, 10);
    cen.writeUInt16LE(now.time, 12);
    cen.writeUInt16LE(now.date, 14);
    cen.writeUInt32LE(crc, 16);
    cen.writeUInt32LE(size, 20);
    cen.writeUInt32LE(size, 24);
    cen.writeUInt16LE(name.length, 28);
    for (let i = 30; i < 46; i++) cen[i] = 0;
    cen.writeUInt32LE(offset, 42);
    central.push(cen, name);
    offset += local.length + name.length + size;
  }
  const cenSize = central.reduce((n, b) => n + b.length, 0);
  const end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0);
  end.writeUInt16LE(0, 4);
  end.writeUInt16LE(0, 6);
  end.writeUInt16LE(entries.length, 8);
  end.writeUInt16LE(entries.length, 10);
  end.writeUInt32LE(cenSize, 12);
  end.writeUInt32LE(offset, 16);
  end.writeUInt16LE(0, 20);
  return Buffer.concat([...parts, ...central, end]);
}

function safeName(n: string): string | null {
  const norm = n.split("\\").join("/").replace(/^\/+/, "");
  const segs = norm.split("/");
  if (segs.some((s) => s === "" || s === "." || s === "..")) return null;
  return norm;
}

async function walk(root: string, out: { rel: string; abs: string }[]): Promise<void> {
  async function rec(dir: string) {
    const ents = await fs.readdir(dir, { withFileTypes: true });
    for (const e of ents) {
      if (e.name === "logs" || e.name === "node_modules") continue;
      const abs = join(dir, e.name);
      if (e.isDirectory()) await rec(abs);
      else out.push({ rel: relative(root, abs).split("\\").join("/"), abs });
    }
  }
  await rec(root);
}

export async function GET(req: Request, { params }: { params: { id: string } }) {
  const job = getJob(params.id);
  if (!job) return NextResponse.json({ error: "unknown job" }, { status: 404 });
  if (!job.converted) return NextResponse.json({ error: "nothing converted yet" }, { status: 400 });
  // ?tree=gentest downloads the staged -out snapshot (sources + tests +
  // gates) instead of the converted tree; provenance lives in the summary.
  const gentestTree = new URL(req.url).searchParams.get("tree") === "gentest";
  const root = gentestTree ? job.gentestOutRoot : job.converted.root;
  if (!root) {
    return NextResponse.json({ error: "no gentest snapshot staged yet — generate with 'staged' on" }, { status: 400 });
  }
  const prefix = gentestTree ? "gentest" : job.converted.target;
  const relRoot = relative(job.dir, root);
  if (relRoot === "" || relRoot.startsWith("..")) {
    return NextResponse.json({ error: "tree escapes the job directory" }, { status: 400 });
  }

  const found: { rel: string; abs: string }[] = [];
  try {
    await walk(root, found);
  } catch {
    return NextResponse.json({ error: "cannot list converted tree" }, { status: 500 });
  }
  found.sort((a, b) => a.rel.localeCompare(b.rel));

  const entries: { name: string; data: Buffer }[] = [];
  for (const f of found) {
    const name = safeName(`${prefix}/${f.rel}`);
    if (!name) continue;
    try {
      const st = await fs.stat(f.abs);
      if (!st.isFile()) continue;
      const data = await fs.readFile(f.abs);
      entries.push({ name, data });
    } catch {
      continue;
    }
  }
  if (entries.length === 0) {
    return NextResponse.json({ error: "converted tree is empty" }, { status: 400 });
  }

  // The reviewed mapping travels with the tree so the zip is self-describing.
  try {
    if (job.mappingPath) {
      const mp = job.mappingPath;
      if (relative(job.dir, mp) !== "" && !relative(job.dir, mp).startsWith("..")) {
        const st = await fs.stat(mp);
        if (st.isFile()) {
          entries.push({ name: `mapping/${basename(mp)}`, data: await fs.readFile(mp) });
        }
      }
    }
  } catch {
    /* mapping is optional cargo */
  }

  const summary = [
    `tux-migration-tool ${gentestTree ? "gentest snapshot" : "conversion"} bundle`,
    `job: ${job.name} (id ${job.id})`,
    `target: ${job.converted.target}`,
    `tree: ${gentestTree ? "gentest -out snapshot (sources + tests)" : "converted tree"}`,
    `files: ${entries.length}`,
    `llm: requested=${job.converted.llm ? "on" : "off"} effective=${job.converted.llmEffective ? "on" : "off"}${job.converted.llmCalls != null ? ` (${job.converted.llmCalls} calls)` : ""}`,
    job.converted.llmNote ? `llm note: ${job.converted.llmNote}` : null,
    job.gentestSummary
      ? `fixtures: ${job.gentestSummary.fromLog} from log, ${job.gentestSummary.assumed} assumed (of ${job.gentestSummary.fixtures.length})`
      : null,
    job.gentestSummary ? `nice-names: requested=${job.gentestNiceNames ? "on" : "off"} effective=${job.gentestLLMEffective ? "on" : "off"}` : null,
    job.gentestSummary?.gates.length ? `gates: ${job.gentestSummary.gates.join(" | ")}` : null,
    job.gentestSummary?.testsFailed ? `tests: FAILED (see gates — non-fatal by design)` : job.gentestSummary ? `tests: passed (or not run)` : null,
    ``,
    `Attached runtime logs are never bundled: they stay in the disposable job`,
    `tmpdir. Converted files vanish on server restart — this zip is the`,
    `durable copy.`,
    ``,
  ].filter((l): l is string => l !== null).join("\n");
  entries.push({ name: "tux-migration-tool-summary.txt", data: Buffer.from(summary, "utf8") });

  const zip = buildZip(entries);
  const fname = `${job.name.replace(/[^\w.-]+/g, "_")}-${prefix}.zip`;
  return new Response(new Uint8Array(zip), {
    headers: {
      "Content-Type": "application/zip",
      "Content-Disposition": `attachment; filename="${fname}"`,
      "Content-Length": String(zip.length),
    },
  });
}
