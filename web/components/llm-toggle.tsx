"use client";

import * as React from "react";
import { Sparkles } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

export function LLMToggle({
  value,
  onChange,
  disabled,
  id,
  hint,
  noKey,
}: {
  value: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
  id: string;
  hint?: string;
  /** True when the server reports no LLM key — LLM-on will fall back. */
  noKey?: boolean;
}) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      <button
        role="switch"
        aria-checked={value}
        aria-label={id}
        disabled={disabled}
        onClick={() => onChange(!value)}
        className={cn(
          "flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs font-medium transition-colors",
          value
            ? "border-primary bg-primary text-primary-foreground"
            : "border-input bg-background text-muted-foreground hover:bg-accent",
          disabled && "cursor-not-allowed opacity-50"
        )}
      >
        <Sparkles className="h-3 w-3" />
        LLM {value ? "on" : "off"}
      </button>
      {value ? (
        <Badge variant="secondary">AI seam — falls back to deterministic when no key resolves</Badge>
      ) : (
        <Badge variant="outline">deterministic — no key needed</Badge>
      )}
      {value && noKey && (
        <Badge variant="outline" title="The web server reports no VLLM_API_KEY / OPENROUTER_API_KEY — this run will fall back to deterministic output">
          no key — expect fallback
        </Badge>
      )}
      {hint && <span className="text-[11px] text-muted-foreground">{hint}</span>}
    </div>
  );
}
