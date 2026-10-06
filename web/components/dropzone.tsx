"use client";

import * as React from "react";
import { UploadCloud, FileCheck2, TriangleAlert, Files, FolderOpen } from "lucide-react";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";

const ACCEPT = [".pc", ".pcf", ".zip"];
const MAX_FILES = 50;
const MAX_PER_FILE = 5 * 1024 * 1024;
const MAX_TOTAL = 30 * 1024 * 1024;

function fmtSize(n: number): string {
  if (n < 1024) return `${n}B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)}KB`;
  return `${(n / 1024 / 1024).toFixed(1)}MB`;
}

export function Dropzone({
  onFiles,
  onFile,
  disabled,
  compact,
}: {
  onFiles?: (files: File[]) => void;
  onFile?: (f: File) => void;
  disabled?: boolean;
  compact?: boolean;
}) {
  const [over, setOver] = React.useState(false);
  const [reject, setReject] = React.useState("");
  const [picked, setPicked] = React.useState<File[]>([]);
  const input = React.useRef<HTMLInputElement>(null);
  const dirInput = React.useRef<HTMLInputElement>(null);

  function acceptList(list: FileList | File[] | undefined) {
    if (!list || disabled) return;
    const files = Array.from(list);
    if (files.length === 0) return;
    if (files.length > MAX_FILES) {
      setReject(`Too many files (${files.length}) — max ${MAX_FILES} per batch. Split into smaller batches.`);
      return;
    }
    let total = 0;
    for (const f of files) {
      const ok = ACCEPT.some((ext) => f.name.toLowerCase().endsWith(ext));
      if (!ok) {
        setReject(`“${f.name}” is not a .pc/.pcf file (.zip ok for batches) — Pro*C sources only.`);
        return;
      }
      const isZip = f.name.toLowerCase().endsWith(".zip");
      if (!isZip && f.size > MAX_PER_FILE) {
        setReject(`“${f.name}” exceeds the 5MB per-file limit.`);
        return;
      }
      if (isZip && f.size > MAX_TOTAL) {
        setReject(`“${f.name}” exceeds the 30MB zip limit.`);
        return;
      }
      total += f.size;
    }
    if (total > MAX_TOTAL) {
      setReject(`Batch totals ${fmtSize(total)} — max ${fmtSize(MAX_TOTAL)}. Split into smaller batches.`);
      return;
    }
    setReject("");
    setPicked(files);
    if (onFiles) onFiles(files);
    else if (onFile && files[0]) onFile(files[0]);
  }

  const totalPicked = picked.reduce((a, f) => a + f.size, 0);

  return (
    <div>
      <div
        role="button"
        tabIndex={0}
        aria-label="Upload Pro*C files — multi-select or folder ok"
        onKeyDown={(e) => {
          if ((e.key === "Enter" || e.key === " ") && !disabled) input.current?.click();
        }}
        onDragOver={(e) => {
          e.preventDefault();
          setOver(true);
        }}
        onDragLeave={() => setOver(false)}
        onDrop={(e) => {
          e.preventDefault();
          setOver(false);
          acceptList(e.dataTransfer.files);
        }}
        onClick={() => !disabled && input.current?.click()}
        className={cn(
          "flex cursor-pointer flex-col items-center justify-center gap-2 rounded-xl border-2 border-dashed text-center transition-colors",
          compact ? "p-5" : "p-8",
          over ? "border-primary bg-primary/5" : "border-input hover:border-primary/50 hover:bg-accent/50",
          disabled && "cursor-wait opacity-60"
        )}
      >
        <span className="flex h-10 w-10 items-center justify-center rounded-full bg-primary/10">
          {reject ? (
            <TriangleAlert className="h-5 w-5 text-destructive" />
          ) : over ? (
            <FileCheck2 className="h-5 w-5 text-primary" />
          ) : (
            <UploadCloud className="h-5 w-5 text-muted-foreground" />
          )}
        </span>
        <p className="text-sm font-medium">
          Drag &amp; drop <code className="rounded bg-muted px-1 font-mono">.pc</code> /{" "}
          <code className="rounded bg-muted px-1 font-mono">.pcf</code> files here, or{" "}
          <span className="text-primary underline underline-offset-2">browse</span>
        </p>
        <p className="max-w-md text-xs text-muted-foreground">
          Multi-select or a whole folder works — up to {MAX_FILES} files, 5MB each / 30MB total. A{" "}
          <code className="rounded bg-muted px-1 font-mono">.zip</code> of sources works too. Parsed locally —
          deterministic, no LLM calls on upload.
        </p>
        {picked.length > 1 && (
          <p className="flex items-center gap-1.5 text-xs" aria-live="polite">
            <Files className="h-3.5 w-3.5 text-primary" />
            {picked.length} files · {fmtSize(totalPicked)}
          </p>
        )}
        <div className="flex gap-2" onClick={(e) => e.stopPropagation()}>
          <Button size="sm" variant="outline" disabled={disabled} onClick={() => input.current?.click()}>
            Select files
          </Button>
          <Button size="sm" variant="ghost" disabled={disabled} onClick={() => dirInput.current?.click()} title="Pick a whole folder">
            <FolderOpen className="h-3.5 w-3.5" />
            Folder
          </Button>
        </div>
        <input
          ref={input}
          type="file"
          accept={ACCEPT.join(",")}
          multiple
          className="hidden"
          onChange={(e) => {
            acceptList(e.target.files ?? undefined);
            e.target.value = "";
          }}
        />
        <input
          ref={dirInput}
          type="file"
          accept=".pc,.pcf"
          // @ts-expect-error webkitdirectory is non-standard but widely supported
          webkitdirectory=""
          className="hidden"
          onChange={(e) => {
            acceptList(e.target.files ?? undefined);
            e.target.value = "";
          }}
        />
      </div>
      {picked.length > 0 && !reject && !compact && (
        <div className="mt-2 flex flex-wrap gap-1.5" aria-live="polite">
          {picked.slice(0, 8).map((f) => (
            <Badge key={f.name} variant="secondary" className="max-w-[220px] truncate font-mono text-[10px]" title={`${f.name} · ${fmtSize(f.size)}`}>
              {f.name}
            </Badge>
          ))}
          {picked.length > 8 && <Badge variant="outline">+{picked.length - 8} more</Badge>}
          <Button size="sm" variant="ghost" className="h-6 px-2 text-xs" onClick={() => setPicked([])}>
            Clear
          </Button>
        </div>
      )}
      {reject && (
        <div className="mt-2 flex items-center justify-between gap-2 rounded-md border border-destructive/40 bg-destructive/5 px-3 py-2 text-xs text-destructive">
          <span>{reject}</span>
          <Button size="sm" variant="ghost" className="h-6 px-2 text-xs" onClick={() => setReject("")}>
            Dismiss
          </Button>
        </div>
      )}
    </div>
  );
}
