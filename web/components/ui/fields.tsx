// Back-compat shim: prefer the canonical primitives directly.
// - `Textarea` → `@/components/ui/textarea`
// - `Input` → `@/components/ui/input`
// - `NativeSelect` → plain <select> styled like shadcn (for simple cases).
// - Radix `Select` → `@/components/ui/select`
import * as React from "react";
import { cn } from "@/lib/utils";

export { Textarea } from "@/components/ui/textarea";
export { Input } from "@/components/ui/input";

export function Select({ className, ...props }: React.SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <select
      className={cn(
        "flex h-9 rounded-md border border-input bg-background px-3 py-1 text-sm shadow-sm focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring",
        className
      )}
      {...props}
    />
  );
}

export const NativeSelect = Select;
