"use client";

import { Database, FileCode2, ArrowRight, Container, Braces } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { ThemeToggle } from "@/components/theme-toggle";

export function SiteHeader({
  jobName,
  status,
  llmMapping,
  llmConvert,
  dbEnabled,
  llmKey,
}: {
  jobName?: string;
  status?: string;
  llmMapping?: boolean;
  llmConvert?: boolean;
  dbEnabled?: boolean;
  /** Null = unknown yet; true = a key resolves; false = LLM-on will fall back. */
  llmKey?: boolean | null;
}) {
  return (
    <header className="sticky top-0 z-40 w-full border-b bg-background/95 backdrop-blur supports-[backdrop-filter]:bg-background/60">
      <div className="container flex h-14 max-w-7xl items-center gap-3">
        <div className="flex h-8 w-8 items-center justify-center rounded-lg bg-primary text-primary-foreground">
          <Container className="h-4 w-4" />
        </div>
        <div className="flex flex-col leading-none">
          <span className="text-sm font-bold tracking-tight">tux-migration-tool</span>
          <span className="text-[11px] text-muted-foreground">Tuxedo Pro*C → Go · Python · C#</span>
        </div>
        <div className="ml-2 hidden items-center gap-1 text-xs text-muted-foreground md:flex">
          <FileCode2 className="h-3.5 w-3.5" />
          <span>tree-sitter parse</span>
          <ArrowRight className="h-3 w-3" />
          <Database className="h-3.5 w-3.5" />
          <span>IR</span>
          <ArrowRight className="h-3 w-3" />
          <Braces className="h-3.5 w-3.5" />
          <span>plan → code → tests</span>
        </div>
        <div className="ml-auto flex items-center gap-2">
          <Badge variant="outline" title="Step 1 mapping LLM seam">
            map LLM {llmMapping ? "on" : "off"}
          </Badge>
          <Badge variant="outline" title="Step 2 convert LLM seam">
            convert LLM {llmConvert ? "on" : "off"}
          </Badge>
          {llmKey != null && (
            <Badge
              variant={llmKey ? "default" : "secondary"}
              title={
                llmKey
                  ? "An LLM API key resolves in the server env — LLM-on runs reach the model"
                  : "No VLLM_API_KEY / OPENROUTER_API_KEY in the server env — LLM-on runs fall back to deterministic output"
              }
            >
              llm key {llmKey ? "ok" : "missing"}
            </Badge>
          )}
          <Badge variant={dbEnabled ? "default" : "secondary"} title="Oracle access (internal/db) — offline unless a DSN is set">
            db {dbEnabled ? "on" : "off"}
          </Badge>
          {jobName && (
            <Badge variant="secondary" className="max-w-[220px] truncate font-mono">
              {jobName}
            </Badge>
          )}
          {status && <Badge variant={status === "error" ? "destructive" : "default"}>{status}</Badge>}
          <ThemeToggle />
        </div>
      </div>
    </header>
  );
}
