import { NextResponse } from "next/server";
import { dsnResolves, readServerConfigSnapshot } from "@/lib/server-config";

// GET /api/db-status — optional Oracle connectivity summary (internal/db).
// Never requires credentials: reports enabled=false when no DSN resolves,
// which is the normal offline mode. Resolution mirrors the CLI
// (internal/db ResolveDSN via database.dsnEnv / database.dsn): the env var
// wins, the TUXCONV_CONFIG yaml literal is the fallback. The DSN value is
// never read out — only booleans and the source channel.
export async function GET() {
  const snap = readServerConfigSnapshot();
  const dsnEnv = (snap?.database.dsnEnv || process.env.ORACLE_DSN_ENV || "ORACLE_DSN").trim() || "ORACLE_DSN";
  const literal = snap?.database.hasLiteralDsn ?? false;
  const enabled = dsnResolves(dsnEnv, literal);
  const viaEnv = (process.env[dsnEnv] ?? "").trim() !== "";
  return NextResponse.json({
    enabled,
    driver: "oracle",
    source: enabled ? (viaEnv ? `env:${dsnEnv}` : "yaml-literal") : "none",
    config: snap ? "yaml (TUXCONV_CONFIG)" : "stock defaults",
    note: enabled
      ? "DSN resolves — live Oracle available (run `tuxconv dbcheck -ping` to verify)"
      : "offline — no DSN in server env nor the TUXCONV_CONFIG yaml; conversion runs deterministic without it",
  });
}
