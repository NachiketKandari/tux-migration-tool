import * as React from "react";
import { cn } from "@/lib/utils";

function ScrollArea({ className, children }: { className?: string; children: React.ReactNode }) {
  return <div className={cn("overflow-auto rounded-md border bg-muted/20", className)}>{children}</div>;
}

export { ScrollArea };
