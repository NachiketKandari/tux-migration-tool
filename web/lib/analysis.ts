// Shared triage-CSV parser: `tuxconv analyze -csv` writes
// `# tuxconv marks:` + marks row + header + one row per file.
// Note: complexity_score / complexity cells are spreadsheet FORMULAS
// (=C$2*C4+... / =IF(...)), so the web recomputes score + tier from the
// marks row and the numeric factor columns — same rubric as the CLI.
import type { AnalysisRow } from "@/lib/jobs";

function splitCsv(line: string): string[] {
  const out: string[] = [];
  let cur = "";
  let inQ = false;
  for (let i = 0; i < line.length; i++) {
    const c = line[i];
    if (inQ) {
      if (c === '"') {
        if (line[i + 1] === '"') {
          cur += '"';
          i++;
        } else {
          inQ = false;
        }
      } else {
        cur += c;
      }
    } else if (c === '"') {
      inQ = true;
    } else if (c === ",") {
      out.push(cur);
      cur = "";
    } else {
      cur += c;
    }
  }
  out.push(cur);
  return out;
}

const num = (v: string | undefined): number => {
  const n = Number((v ?? "").trim());
  return Number.isFinite(n) ? n : 0;
};

function marksOf(header: string[], marksRow: string[]): { q: number; branch: number; tpcall: number; high: number; med: number } {
  const at = (name: string) => {
    const i = header.indexOf(name);
    return i >= 0 ? num(marksRow[i]) : 0;
  };
  return {
    q: at("num_queries") || 1,
    branch: at("branching_factor") || 1,
    tpcall: at("tpcall_count") || 20,
    high: at("complexity_score") || 30,
    med: at("complexity") || 10,
  };
}

export function parseTriageCsv(csv: string): AnalysisRow[] {
  const lines = csv.split("\n").filter((l) => l.trim() !== "");
  if (lines.length < 4) return [];
  // Line 0 = `# tuxconv marks:`, line 1 = marks cells row, line 2 = header.
  const header = splitCsv(lines[2]).map((h) => h.trim());
  const marks = marksOf(header, splitCsv(lines[1]));
  const idx = (name: string) => header.indexOf(name);
  const rows: AnalysisRow[] = [];
  for (const line of lines.slice(3)) {
    const cols = splitCsv(line);
    const file = cols[idx("file")] ?? "";
    if (!file) continue;
    const nq = num(cols[idx("num_queries")]);
    const bf = num(cols[idx("branching_factor")]);
    const tpc = num(cols[idx("tpcall_count")]);
    const extW = num(cols[idx("external_weight")]);
    const score = marks.q * nq + marks.branch * bf + marks.tpcall * tpc + extW;
    const tier = score >= marks.high ? "HIGH" : score >= marks.med ? "MEDIUM" : "LOW";
    rows.push({
      file,
      numLines: num(cols[idx("num_lines")]),
      numQueries: nq,
      branchingFactor: bf,
      branchCount: num(cols[idx("branch_count")]),
      hasTpcall: /^(true|1|t|yes)$/i.test((cols[idx("has_tpcall")] ?? "").trim()),
      tpcallCount: tpc,
      fnLocalCount: num(cols[idx("fn_local_count")]),
      fnExternalCount: num(cols[idx("fn_external_count")]),
      complexityScore: score,
      complexity: tier,
      reasons: (cols[idx("reasons")] ?? "").trim(),
    });
  }
  // Highest effort first — the conversion order answer.
  rows.sort((a, b) => b.complexityScore - a.complexityScore);
  return rows;
}
