"use client";

import * as React from "react";
import type { LineageResult } from "@/lib/lineage";

export interface LineageState extends LineageResult {
  target?: string;
}

/** Fetch the source→output trace; refetches when the converted tree changes. */
export function useLineage(jobId: string | null, stamp: string) {
  const [data, setData] = React.useState<LineageState | null>(null);
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState("");

  React.useEffect(() => {
    if (!jobId || !stamp) {
      setData(null);
      setError("");
      return;
    }
    let live = true;
    setLoading(true);
    setError("");
    fetch(`/api/jobs/${jobId}/lineage`)
      .then(async (r) => {
        const d = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error((d as { error?: string }).error ?? "lineage failed");
        if (live) setData(d as LineageState);
      })
      .catch((e) => {
        if (live) {
          setError(e instanceof Error ? e.message : "lineage failed");
          setData(null);
        }
      })
      .finally(() => {
        if (live) setLoading(false);
      });
    return () => {
      live = false;
    };
  }, [jobId, stamp]);

  return { lineage: data, loading, error };
}
