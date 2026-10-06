import { randomUUID } from "node:crypto";
import { spawn } from "node:child_process";
import { promises as fs } from "node:fs";
import { join, basename } from "node:path";
import { NextResponse } from "next/server";
import { setJob, makeLogger, type Job } from "@/lib/jobs";
import { coerceFlowReport } from "@/lib/flow";
import { parseTriageCsv } from "@/lib/analysis";
import { countQueries, recordMetric } from "@/lib/metrics";
import { newJobDir, runTux } from "@/lib/tuxconv";

const MAX_FILES = 50;
const MAX_PER_FILE = 5 * 1024 * 1024;
const MAX_TOTAL = 30 * 1024 * 1024;

function safeBase(name: string): string {
  const b = basename(name || "upload.pc").replace(/[^\w.\-+@]+/g, "_");
  return b || "upload.pc";
}

function unzipList(zipPath: string, destDir: string): Promise<{ code: number; stderr: string }> {
  return new Promise((resolve) => {
    const child = spawn("unzip", ["-o", "-j", zipPath, "-d", destDir], { timeout: 60_000 });
    let stderr = "";
    child.stderr.on("data", (d: Buffer) => {
      stderr += d.toString();
    });
    child.on("error", (err) => resolve({ code: 1, stderr: String(err) }));
    child.on("close", (code) => resolve({ code: code ?? 1, stderr }));
  });
}

export async function POST(req: Request) {
  const form = await req.formData();
  // New multi-file field ("files") + legacy single ("file") for compat.
  const incoming: File[] = [];
  for (const v of form.getAll("files")) if (v instanceof File) incoming.push(v);
  const legacy = form.get("file");
  if (legacy instanceof File) incoming.push(legacy);
  if (incoming.length === 0) {
    return NextResponse.json({ error: "field 'files' is required (.pc/.pcf, multi-select or folder ok; .zip ok)" }, { status: 400 });
  }
  if (incoming.length > MAX_FILES) {
    return NextResponse.json({ error: `too many files (${incoming.length}) — max ${MAX_FILES} per job; split into smaller batches` }, { status: 400 });
  }

  const dir = await newJobDir();
  const inputDir = join(dir, "input");
  await fs.mkdir(inputDir, { recursive: true });

  const saved: string[] = [];
  let total = 0;
  try {
    // .zip batch: one archive carrying many .pc/.pcf files.
    if (incoming.length === 1 && /\.zip$/i.test(incoming[0].name || "")) {
      const zf = incoming[0];
      if (zf.size > MAX_TOTAL) {
        return NextResponse.json({ error: `"${zf.name}" exceeds the 30MB zip limit.` }, { status: 400 });
      }
      const zipPath = join(dir, safeBase(zf.name));
      await fs.writeFile(zipPath, Buffer.from(await zf.arrayBuffer()));
      const r = await unzipList(zipPath, inputDir);
      if (r.code !== 0) {
        return NextResponse.json(
          { error: "could not unzip the archive (needs the `unzip` binary server-side) — or multi-select the .pc files directly" },
          { status: 400 }
        );
      }
      await fs.rm(zipPath, { force: true });
      // Keep only Pro*C sources; drop __MACOSX / stray files.
      for (const n of await fs.readdir(inputDir)) {
        const p = join(inputDir, n);
        try {
          const st = await fs.stat(p);
          if (!st.isFile() || !/\.(pc|pcf)$/i.test(n)) await fs.rm(p, { force: true, recursive: true });
        } catch {
          continue;
        }
      }
      for (const n of await fs.readdir(inputDir)) {
        if (/\.(pc|pcf)$/i.test(n)) saved.push(n);
      }
      if (saved.length === 0) {
        return NextResponse.json({ error: "zip held no .pc/.pcf files" }, { status: 400 });
      }
      if (saved.length > MAX_FILES) {
        return NextResponse.json({ error: `zip held ${saved.length} sources — max ${MAX_FILES} per job` }, { status: 400 });
      }
    } else {
      const seen = new Set<string>();
      for (const f of incoming) {
        const raw = f.name || "upload.pc";
        if (!/\.(pc|pcf)$/i.test(raw)) {
          return NextResponse.json({ error: `“${raw}” is not a .pc/.pcf file — Pro*C sources only (.zip ok for batches).` }, { status: 400 });
        }
        if (f.size > MAX_PER_FILE) {
          return NextResponse.json({ error: `“${raw}” exceeds the 5MB per-file limit.` }, { status: 400 });
        }
        total += f.size;
        if (total > MAX_TOTAL) {
          return NextResponse.json({ error: `batch exceeds the 30MB total limit — split into smaller batches.` }, { status: 400 });
        }
        let base = safeBase(raw);
        let i = 1;
        while (seen.has(base.toLowerCase())) {
          base = base.replace(/(\.[^.]+)?$/, `_${i}$1`);
          i++;
        }
        seen.add(base.toLowerCase());
        await fs.writeFile(join(inputDir, base), Buffer.from(await f.arrayBuffer()));
        saved.push(base);
      }
    }
  } catch (e) {
    return NextResponse.json({ error: e instanceof Error ? e.message : "upload failed" }, { status: 500 });
  }
  saved.sort((a, b) => a.localeCompare(b));

  const isBatch = saved.length > 1;
  const sourcePath = isBatch ? inputDir : join(inputDir, saved[0]);
  const name = isBatch ? `${saved.length} files (${saved[0]} +${saved.length - 1})` : saved[0];

  const job: Job = {
    id: randomUUID().slice(0, 8),
    name,
    dir,
    sourcePath,
    inputFiles: saved,
    isBatch,
    status: "running",
    logs: [],
  };
  setJob(job);
  const log = makeLogger(job);
  log(`$ upload ${saved.length} file(s): ${saved.slice(0, 5).join(", ")}${saved.length > 5 ? ` +${saved.length - 5} more` : ""}`);

  try {
    if (!isBatch) {
      const irPath = join(dir, "ir.json");
      const ex = await runTux(dir, ["extract", sourcePath, "-out", irPath], log);
      if (ex.code !== 0) throw new Error("extract failed — see logs");
      job.ir = JSON.parse(await fs.readFile(irPath, "utf8"));
    } else {
      const irDir = join(dir, "ir");
      const ex = await runTux(dir, ["extract", sourcePath, "-out", irDir], log);
      if (ex.code !== 0) throw new Error("extract failed — see logs");
      const irList: { name: string; ir: Record<string, unknown> }[] = [];
      for (const n of await fs.readdir(irDir)) {
        if (!/\.ir\.json$/i.test(n)) continue;
        try {
          const ir = JSON.parse(await fs.readFile(join(irDir, n), "utf8"));
          const src = saved.find((s) => n.startsWith(s.replace(/\.[^.]+$/, ""))) ?? n;
          irList.push({ name: src, ir });
        } catch {
          continue;
        }
      }
      irList.sort((a, b) => a.name.localeCompare(b.name));
      job.irList = irList;
      job.ir = irList[0]?.ir;
      if (!job.ir) throw new Error("extract wrote no IR — see logs");
    }

    const fl = await runTux(dir, ["flow", sourcePath], log);
    job.flowText = fl.stdout || "(no flow output)";

    try {
      const flowPath = join(dir, "flow.json");
      const fr = await runTux(dir, ["flow", sourcePath, "-out", flowPath], log);
      if (fr.code === 0) {
        const raw = JSON.parse(await fs.readFile(flowPath, "utf8"));
        job.flowReport = coerceFlowReport(raw, sourcePath);
      }
    } catch {
      /* keep flowText-only */
    }

    // Analyze (triage rubric): runs on file or dir, never touches the LLM.
    try {
      const csvPath = join(dir, "triage.csv");
      const an = await runTux(dir, ["analyze", sourcePath, "-csv", csvPath], log);
      if (an.code === 0) {
        const csv = await fs.readFile(csvPath, "utf8");
        job.analysisCsv = csv.length > 256 * 1024 ? csv.slice(0, 256 * 1024) + "\n…[truncated]" : csv;
        job.analysis = parseTriageCsv(csv).map((r) => ({ ...r, file: basename(r.file) }));
      }
    } catch {
      /* analysis is advisory */
    }

    if (!isBatch) {
      try {
        const src = await fs.readFile(sourcePath, "utf8");
        job.sourcePreview = src.length > 24 * 1024 ? src.slice(0, 24 * 1024) + "\n…[truncated]" : src;
      } catch {
        /* preview is optional */
      }
    } else {
      const previews: { name: string; preview: string }[] = [];
      for (const n of saved.slice(0, 20)) {
        try {
          const src = await fs.readFile(join(inputDir, n), "utf8");
          previews.push({ name: n, preview: src.length > 8 * 1024 ? src.slice(0, 8 * 1024) + "\n…[truncated]" : src });
        } catch {
          continue;
        }
      }
      job.sourcePreviews = previews;
      job.sourcePreview = previews[0] ? `// ${previews[0].name}\n${previews[0].preview}` : undefined;
    }

    job.status = "done";
    const totalQueries = job.irList ? job.irList.reduce((a, e) => a + countQueries(e.ir), 0) : countQueries(job.ir);
    await recordMetric({ kind: "parse", job: job.name, queries: totalQueries, note: isBatch ? `${saved.length} files` : undefined });
  } catch (e) {
    job.status = "error";
    job.error = e instanceof Error ? e.message : "upload pipeline failed";
  }
  setJob(job);
  return NextResponse.json({ id: job.id, status: job.status, error: job.error });
}
