"use client";

import { Check, Lock } from "lucide-react";
import { cn } from "@/lib/utils";

export const PIPELINE_STEPS = [
  { id: "upload", label: "Upload", hint: "Files land here" },
  { id: "overview", label: "Overview", hint: "KPIs + analyze" },
  { id: "scenarios", label: "Scenarios", hint: "Dispatch slices" },
  { id: "mapping", label: "Mapping", hint: "Step 1 draft" },
  { id: "convert", label: "Convert", hint: "Step 2 code" },
  { id: "tests", label: "Tests", hint: "Gentest gaps" },
] as const;

export function PipelineSteps({
  active,
  done,
  blocked,
  onGo,
}: {
  active: string;
  done: Set<string>;
  blocked?: Set<string>;
  onGo: (id: string) => void;
}) {
  return (
    <ol className="flex flex-wrap items-center gap-1.5" aria-label="Pipeline progress">
      {PIPELINE_STEPS.map((s, i) => {
        const isDone = done.has(s.id) || (s.id === "upload" && done.size > 0);
        const isActive = active === s.id;
        const isBlocked = blocked?.has(s.id) ?? false;
        return (
          <li key={s.id} className="flex items-center gap-1.5">
            {i > 0 && <span className="mx-0.5 h-px w-4 bg-border" aria-hidden />}
            <button
              onClick={() => !isBlocked && onGo(s.id)}
              disabled={isBlocked}
              title={isBlocked ? `${s.label} unlocks after the previous step` : `${s.label} — ${s.hint}`}
              aria-current={isActive ? "step" : undefined}
              className={cn(
                "flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs font-medium transition-colors",
                isActive
                  ? "border-primary bg-primary text-primary-foreground"
                  : isBlocked
                    ? "cursor-not-allowed border-input bg-muted/40 text-muted-foreground/60"
                    : isDone
                      ? "border-primary/40 bg-primary/10 text-foreground hover:bg-primary/15"
                      : "border-input bg-background text-muted-foreground hover:bg-accent"
              )}
            >
              {isBlocked ? (
                <Lock className="h-3 w-3" />
              ) : isDone && !isActive ? (
                <Check className="h-3 w-3" />
              ) : (
                <span className="tabular-nums">{i + 1}</span>
              )}
              {s.label}
            </button>
          </li>
        );
      })}
    </ol>
  );
}
