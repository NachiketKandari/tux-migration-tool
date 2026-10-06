"use client";

import * as React from "react";
import {
  LayoutDashboard,
  Gauge,
  FileJson,
  GitBranch,
  GitFork,
  PencilLine,
  Hammer,
  FlaskConical,
  ScrollText,
  FileCode2,
  Loader2,
  Activity,
  Waypoints,
  ArrowRight,
  ArrowLeft,
  Check,
} from "lucide-react";
import { SiteHeader } from "@/components/site-header";
import { PipelineSteps } from "@/components/pipeline-steps";
import { Dropzone } from "@/components/dropzone";
import { SamplesPanel } from "@/components/samples-panel";
import { OverviewDashboard } from "@/components/overview-dashboard";
import { AnalyzePanel } from "@/components/analyze-panel";
import { IRExplorer } from "@/components/ir-explorer";
import { FlowExplorer } from "@/components/flow-explorer";
import { ScenarioExplorer } from "@/components/scenario-explorer";
import { MappingEditor } from "@/components/mapping-editor";
import { ConvertPanel } from "@/components/convert-panel";
import { LineageExplorer } from "@/components/lineage-explorer";
import { TestsPanel } from "@/components/tests-panel";
import { MetricsPanel } from "@/components/metrics-panel";
import { CodeView } from "@/components/files";
import { Skeleton } from "@/components/ui/skeleton";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { useJob } from "@/hooks/use-job";
import { useLineage } from "@/hooks/use-lineage";
import { provenanceForFile } from "@/lib/lineage";
import {
  uploadFiles,
  loadSample,
  discover,
  buildScenarios,
  saveMapping,
  convert,
  gentest,
  uploadGentestLog,
  clearGentestLog,
  runAnalysis,
  readConvertedFile,
  saveConvertedFile,
  fetchDbStatus,
  fetchLLMStatus,
  type LLMStatus,
  type GentestOptions,
} from "@/lib/api-client";
import type { ConvertTarget } from "@/lib/targets";

type StepId = "upload" | "overview" | "scenarios" | "mapping" | "convert" | "tests";
type OverviewSub = "dashboard" | "analyze" | "ir" | "flow" | "source";
type ConvertSub = "build" | "trace" | "logs";
type TestsSub = "tests" | "metrics";

const STEP_ORDER: StepId[] = ["upload", "overview", "scenarios", "mapping", "convert", "tests"];

function nextStep(s: StepId): StepId | null {
  const i = STEP_ORDER.indexOf(s);
  return i >= 0 && i < STEP_ORDER.length - 1 ? STEP_ORDER[i + 1] : null;
}
function prevStep(s: StepId): StepId | null {
  const i = STEP_ORDER.indexOf(s);
  return i > 0 ? STEP_ORDER[i - 1] : null;
}

export default function Home() {
  const [jobId, setJobId] = React.useState<string | null>(null);
  const { job, setJob, logs, resetLogs } = useJob(jobId);
  const [step, setStep] = React.useState<StepId>("upload");
  const [overviewSub, setOverviewSub] = React.useState<OverviewSub>("dashboard");
  const [convertSub, setConvertSub] = React.useState<ConvertSub>("build");
  const [testsSub, setTestsSub] = React.useState<TestsSub>("tests");
  const [busy, setBusy] = React.useState(false);
  const [err, setErr] = React.useState("");
  const [target, setTarget] = React.useState<ConvertTarget>("go");
  const [useLLMMapping, setUseLLMMapping] = React.useState(false);
  const [useLLMConvert, setUseLLMConvert] = React.useState(false);
  const [dbStatus, setDbStatus] = React.useState<{ enabled: boolean; driver: string; source: string } | null>(null);
  const [llmStatus, setLlmStatus] = React.useState<LLMStatus | null>(null);
  const [selFile, setSelFile] = React.useState<string | null>(null);
  const [fileContent, setFileContent] = React.useState("");
  const [loadingFile, setLoadingFile] = React.useState(false);
  const [savingFile, setSavingFile] = React.useState(false);
  const [confirmReplace, setConfirmReplace] = React.useState<File[] | null>(null);

  const lineageStamp = job?.converted ? `${job.id}:${job.converted.target}:${job.converted.files.length}` : "";
  const { lineage, loading: lineageLoading, error: lineageError } = useLineage(jobId, lineageStamp);
  const selProvenance = React.useMemo(
    () => (selFile ? provenanceForFile(lineage, selFile) : []),
    [lineage, selFile]
  );

  React.useEffect(() => {
    fetchDbStatus().then(setDbStatus).catch(() => setDbStatus(null));
    fetchLLMStatus().then(setLlmStatus).catch(() => setLlmStatus(null));
  }, []);

  async function run<T>(fn: () => Promise<T>): Promise<T | null> {
    setBusy(true);
    setErr("");
    try {
      return await fn();
    } catch (e) {
      setErr(e instanceof Error ? e.message : "request failed");
      return null;
    } finally {
      setBusy(false);
      if (jobId) {
        const r = await fetch(`/api/jobs/${jobId}`);
        if (r.ok) setJob(await r.json());
      }
    }
  }

  async function doUpload(files: File[]) {
    resetLogs();
    setSelFile(null);
    setConfirmReplace(null);
    const d = await run(() => uploadFiles(files));
    if (d) {
      setJobId(d.id);
      setStep("overview");
      setOverviewSub("dashboard");
    }
  }

  function handlePick(files: File[]) {
    // Uploading again replaces the current batch — confirm so work isn't lost silently.
    if (job && !confirmReplace) {
      setConfirmReplace(files);
      return;
    }
    void doUpload(files);
  }

  async function handleSample(path: string) {
    resetLogs();
    setSelFile(null);
    setConfirmReplace(null);
    const d = await run(() => loadSample(path));
    if (d) {
      setJobId(d.id);
      setStep("overview");
      setOverviewSub("dashboard");
    }
  }

  async function handleAnalyze() {
    if (!job) return;
    const j = await run(() => runAnalysis(job.id));
    if (j) {
      setJob(j);
      setOverviewSub("analyze");
    }
  }

  async function handleDiscover(t: "go" | "cs") {
    if (!job) return;
    const j = await run(() => discover(job.id, t, useLLMMapping));
    if (j) {
      setJob(j);
      setStep("mapping");
    }
  }

  async function handleScenarios() {
    if (!job) return;
    const j = await run(() => buildScenarios(job.id));
    if (j) setJob(j);
  }

  async function handleSaveMapping(path: string, content: string) {
    if (!job) return;
    const ok = await run(async () => {
      await saveMapping(job.id, path, content);
      return true;
    });
    if (ok) {
      // Batch: one save covers one draft — stay so the rest can be reviewed.
      if (job.isBatch && (job.drafts?.length ?? 0) > 1) {
        const r = await fetch(`/api/jobs/${job.id}`);
        if (r.ok) setJob(await r.json());
        return;
      }
      setStep("convert");
    }
  }

  async function handleConvert() {
    if (!job) return;
    if ((target === "go" || target === "cs") && !job.mappingPath) {
      setStep("mapping");
      setErr("Step 1 first — draft a mapping, save it, then convert (Step 2).");
      return;
    }
    setSelFile(null);
    setFileContent("");
    const res = await run(() => convert(job.id, target, useLLMConvert));
    if (res) {
      setJob(res.job);
      if (!res.job.converted && (res.note || res.drafts)) {
        setStep("mapping");
        setErr("No mapping yet — review the draft in the Mapping step, save, then convert again.");
      } else {
        setConvertSub("build");
      }
    }
  }

  async function handleGentest(mode: "check" | "generate", opts?: GentestOptions) {
    if (!job) return;
    const j = await run(() => gentest(job.id, mode, opts));
    if (j) setJob(j);
  }

  async function handleUploadLog(file: File) {
    if (!job) return;
    const j = await run(() => uploadGentestLog(job.id, file));
    if (j) setJob(j);
  }

  async function handleClearLog() {
    if (!job) return;
    const j = await run(() => clearGentestLog(job.id));
    if (j) setJob(j);
  }

  async function openFile(p: string) {
    if (!job) return;
    setSelFile(p);
    setLoadingFile(true);
    try {
      setFileContent(await readConvertedFile(job.id, p));
    } catch (e) {
      setFileContent(`// ${e instanceof Error ? e.message : "cannot read file"}`);
    } finally {
      setLoadingFile(false);
    }
  }

  async function openTraceFile(p: string) {
    setConvertSub("build");
    await openFile(p);
  }

  async function handleSaveFile(p: string, content: string) {
    if (!job) return;
    setSavingFile(true);
    try {
      await saveConvertedFile(job.id, p, content);
      setFileContent(content);
    } catch (e) {
      setErr(e instanceof Error ? e.message : "save failed");
    } finally {
      setSavingFile(false);
    }
  }

  const done = React.useMemo(() => {
    const s = new Set<string>();
    if (!job) return s;
    s.add("upload");
    if (job.ir || (job.irList && job.irList.length > 0)) {
      s.add("overview");
    }
    if (job.analysis && job.analysis.length > 0) s.add("overview");
    if (job.scenarios) s.add("scenarios");
    if (job.drafts?.length || job.mappingPath) s.add("mapping");
    if (job.converted) s.add("convert");
    if (job.gentestGap) s.add("tests");
    return s;
  }, [job]);

  const blocked = React.useMemo(() => {
    const s = new Set<string>();
    if (!job) {
      for (const id of ["overview", "scenarios", "mapping", "convert", "tests"]) s.add(id);
    }
    return s;
  }, [job]);

  const nxt = nextStep(step);
  const prv = prevStep(step);

  return (
    <div className="min-h-screen">
      <SiteHeader
        jobName={job?.name}
        status={job?.status}
        llmMapping={useLLMMapping}
        llmConvert={useLLMConvert}
        dbEnabled={dbStatus?.enabled ?? false}
        llmKey={llmStatus ? llmStatus.anyKey : null}
      />

      <main className="container max-w-7xl py-6">
        <div className="mb-5 flex flex-wrap items-end justify-between gap-3">
          <div>
            <h1 className="text-2xl font-bold tracking-tight">Drop Pro*C files — watch them become APIs</h1>
            <p className="mt-1 max-w-2xl text-sm text-muted-foreground">
              Batch upload → Overview + effort analysis → dispatch-axis scenarios →{" "}
              <span className="font-medium text-foreground">Step 1 mapping</span> →{" "}
              <span className="font-medium text-foreground">Step 2 Tux → Go · Python · C#</span> → tests. LLM off
              works with no keys; LLM on upgrades where a key resolves. Oracle access is optional.
            </p>
          </div>
          {job && (
            <div className="flex items-center gap-2">
              <Badge variant="secondary" className="font-mono">
                {job.name}
              </Badge>
              {job.isBatch && job.inputFiles && (
                <Badge variant="outline" className="tabular-nums">
                  {job.inputFiles.length} files
                </Badge>
              )}
              {busy && (
                <span className="flex items-center gap-1 text-xs text-muted-foreground">
                  <Loader2 className="h-3.5 w-3.5 animate-spin" /> working…
                </span>
              )}
            </div>
          )}
        </div>

        <div className="mb-4 flex flex-wrap items-center gap-3">
          <PipelineSteps active={step} done={done} blocked={blocked} onGo={(id) => setStep(id as StepId)} />
          {job && (
            <div className="ml-auto flex gap-1.5">
              {prv && prv !== "upload" && (
                <Button size="sm" variant="ghost" onClick={() => setStep(prv)}>
                  <ArrowLeft className="h-3.5 w-3.5" /> Back
                </Button>
              )}
              {nxt && done.has(step) && (
                <Button size="sm" variant="outline" onClick={() => setStep(nxt)}>
                  Next: {nxt} <ArrowRight className="h-3.5 w-3.5" />
                </Button>
              )}
            </div>
          )}
        </div>

        {step === "upload" && !job && (
          <>
            {busy ? (
              <div className="grid grid-cols-1 gap-3 lg:grid-cols-[1fr_380px]" aria-label="Loading job">
                <Card>
                  <CardContent className="space-y-2 p-4">
                    <Skeleton className="h-5 w-1/3" />
                    <Skeleton className="h-4 w-full" />
                    <Skeleton className="h-4 w-5/6" />
                    <Skeleton className="h-32 w-full" />
                  </CardContent>
                </Card>
                <Card>
                  <CardContent className="space-y-2 p-4">
                    <Skeleton className="h-5 w-1/2" />
                    <Skeleton className="h-8 w-full" />
                    <Skeleton className="h-8 w-full" />
                  </CardContent>
                </Card>
              </div>
            ) : (
              <div className="grid grid-cols-1 gap-3 lg:grid-cols-[1fr_380px]">
                <Dropzone onFiles={handlePick} onFile={(f) => handlePick([f])} disabled={busy} />
                <SamplesPanel busy={busy} onPick={handleSample} />
              </div>
            )}
          </>
        )}

        {(job || step !== "upload") && step === "upload" && job && (
          <Card className="mb-4">
            <CardHeader className="pb-2">
              <CardTitle className="text-sm">Batch contents — {job.inputFiles?.length ?? 1} file(s)</CardTitle>
              <CardDescription>
                One job = one batch. Uploading again starts a fresh batch (current progress is replaced).
              </CardDescription>
            </CardHeader>
            <CardContent className="space-y-2">
              <div className="flex flex-wrap gap-1.5">
                {(job.inputFiles ?? [job.name]).map((n) => (
                  <Badge key={n} variant="secondary" className="max-w-[240px] truncate font-mono text-[10px]" title={n}>
                    {n}
                  </Badge>
                ))}
              </div>
              <div className="flex flex-wrap items-center gap-2 rounded-md border p-3">
                <Dropzone onFiles={handlePick} onFile={(f) => handlePick([f])} disabled={busy} compact />
                <span className="text-xs text-muted-foreground">…or start over with a new batch.</span>
              </div>
              {confirmReplace && (
                <Alert>
                  <AlertDescription className="flex flex-wrap items-center gap-2">
                    <span className="flex-1">
                      Replace this batch ({job.inputFiles?.length ?? 1} file(s)) with {confirmReplace.length}{" "}
                      new file(s)? Mapping / conversion progress on this batch will be lost.
                    </span>
                    <Button size="sm" variant="destructive" disabled={busy} onClick={() => void doUpload(confirmReplace)}>
                      Replace batch
                    </Button>
                    <Button size="sm" variant="ghost" onClick={() => setConfirmReplace(null)}>
                      Keep current
                    </Button>
                  </AlertDescription>
                </Alert>
              )}
              <div>
                <Button size="sm" onClick={() => setStep("overview")}>
                  Continue to Overview <ArrowRight className="h-3.5 w-3.5" />
                </Button>
              </div>
            </CardContent>
          </Card>
        )}

        {err && (
          <Alert variant="destructive" className="mb-4">
            <AlertDescription className="flex flex-wrap items-center gap-2">
              <span className="flex-1">{err}</span>
              <Button size="sm" variant="ghost" className="h-6 px-2 text-xs" onClick={() => setErr("")}>
                Dismiss
              </Button>
            </AlertDescription>
          </Alert>
        )}

        {job && step === "overview" && (
          <div className="space-y-3">
            <Tabs value={overviewSub} onValueChange={(v) => setOverviewSub(v as OverviewSub)}>
              <TabsList className="flex h-auto max-w-full flex-wrap justify-start gap-1 overflow-x-auto">
                <TabsTrigger value="dashboard">
                  <LayoutDashboard className="h-3.5 w-3.5" /> Dashboard
                </TabsTrigger>
                <TabsTrigger value="analyze">
                  <Gauge className="h-3.5 w-3.5" /> Analyze
                  {(job.analysis?.length ?? 0) > 0 && (
                    <Badge variant="secondary" className="ml-1 px-1.5 py-0 text-[10px] tabular-nums">
                      {job.analysis?.length}
                    </Badge>
                  )}
                </TabsTrigger>
                <TabsTrigger value="ir">
                  <FileJson className="h-3.5 w-3.5" /> IR
                </TabsTrigger>
                <TabsTrigger value="flow">
                  <GitBranch className="h-3.5 w-3.5" /> Flow
                </TabsTrigger>
                <TabsTrigger value="source">
                  <FileCode2 className="h-3.5 w-3.5" /> Source
                  {(job.sourcePreviews?.length ?? 0) > 1 && (
                    <Badge variant="secondary" className="ml-1 px-1.5 py-0 text-[10px] tabular-nums">
                      {job.sourcePreviews?.length}
                    </Badge>
                  )}
                </TabsTrigger>
              </TabsList>
              <TabsContent value="dashboard">
                <OverviewDashboard
                  ir={job.ir}
                  irList={job.irList}
                  inputFiles={job.inputFiles}
                  analysis={job.analysis}
                  flowReport={job.flowReport}
                  onGoMetrics={() => {
                    setStep("tests");
                    setTestsSub("metrics");
                  }}
                  onGoTrace={job.converted ? () => { setStep("convert"); setConvertSub("trace"); } : undefined}
                  onGoAnalyze={() => setOverviewSub("analyze")}
                />
                <div className="mt-3 flex gap-2">
                  <Button size="sm" onClick={() => setStep("scenarios")}>
                    Next: Scenarios <ArrowRight className="h-3.5 w-3.5" />
                  </Button>
                  <Button size="sm" variant="outline" onClick={() => setOverviewSub("analyze")}>
                    Effort estimate
                  </Button>
                </div>
              </TabsContent>
              <TabsContent value="analyze">
                <AnalyzePanel jobId={job.id} analysis={job.analysis} csv={job.analysisCsv} busy={busy} onRun={handleAnalyze} />
              </TabsContent>
              <TabsContent value="ir">
                {job.irList && job.irList.length > 1 ? (
                  <div className="space-y-3">
                    {job.irList.map((e) => (
                      <Card key={e.name}>
                        <CardHeader className="pb-2">
                          <CardTitle className="font-mono text-sm">{e.name}</CardTitle>
                        </CardHeader>
                        <CardContent>
                          <IRExplorer ir={e.ir} />
                        </CardContent>
                      </Card>
                    ))}
                  </div>
                ) : (
                  <IRExplorer ir={job.ir} />
                )}
              </TabsContent>
              <TabsContent value="flow">
                <FlowExplorer flowText={job.flowText} flowReport={job.flowReport} />
              </TabsContent>
              <TabsContent value="source">
                {job.sourcePreviews && job.sourcePreviews.length > 1 ? (
                  <div className="space-y-3">
                    {job.sourcePreviews.map((s) => (
                      <Card key={s.name}>
                        <CardHeader className="pb-2">
                          <CardTitle className="font-mono text-sm">{s.name}</CardTitle>
                        </CardHeader>
                        <CardContent className="p-0">
                          <ScrollArea className="max-h-[420px] border-0">
                            <CodeView content={s.preview} path={s.name} />
                          </ScrollArea>
                        </CardContent>
                      </Card>
                    ))}
                  </div>
                ) : (
                  <Card>
                    <CardContent className="p-0">
                      <ScrollArea className="max-h-[560px] border-0">
                        <CodeView content={job.sourcePreview ?? "(no preview)"} path={job.name} />
                      </ScrollArea>
                    </CardContent>
                  </Card>
                )}
              </TabsContent>
            </Tabs>
          </div>
        )}

        {job && step === "scenarios" && (
          <div className="space-y-3">
            <ScenarioExplorer jobId={job.id} bundle={job.scenarios} busy={busy} onBuild={handleScenarios} />
            <div className="flex gap-2">
              <Button size="sm" variant="outline" onClick={() => setStep("overview")}>
                <ArrowLeft className="h-3.5 w-3.5" /> Overview
              </Button>
              <Button size="sm" onClick={() => setStep("mapping")}>
                Next: Mapping <ArrowRight className="h-3.5 w-3.5" />
              </Button>
            </div>
          </div>
        )}

        {job && step === "mapping" && (
          <div className="space-y-3">
            <MappingEditor
              drafts={job.drafts ?? []}
              busy={busy}
              onDraft={handleDiscover}
              onSave={handleSaveMapping}
              useLLM={useLLMMapping}
              onUseLLM={setUseLLMMapping}
              llmEffective={job.mappingLLMEffective}
              llmNote={job.mappingLLMNote}
              noKey={llmStatus ? !llmStatus.anyKey : false}
            />
            <div className="flex gap-2">
              <Button size="sm" variant="outline" onClick={() => setStep("scenarios")}>
                <ArrowLeft className="h-3.5 w-3.5" /> Scenarios
              </Button>
              {job.mappingPath && (
                <Button size="sm" onClick={() => setStep("convert")}>
                  <Check className="h-3.5 w-3.5" /> Saved — Convert <ArrowRight className="h-3.5 w-3.5" />
                </Button>
              )}
            </div>
          </div>
        )}

        {job && step === "convert" && (
          <div className="space-y-3">
            <Tabs value={convertSub} onValueChange={(v) => setConvertSub(v as ConvertSub)}>
              <TabsList className="flex h-auto max-w-full flex-wrap justify-start gap-1 overflow-x-auto">
                <TabsTrigger value="build">
                  <Hammer className="h-3.5 w-3.5" /> Build
                  {(job.converted?.files.length ?? 0) > 0 && (
                    <Badge variant="secondary" className="ml-1 px-1.5 py-0 text-[10px] tabular-nums">
                      {job.converted?.files.length}
                    </Badge>
                  )}
                </TabsTrigger>
                <TabsTrigger value="trace">
                  <Waypoints className="h-3.5 w-3.5" /> Trace
                </TabsTrigger>
                <TabsTrigger value="logs">
                  <ScrollText className="h-3.5 w-3.5" /> Logs
                </TabsTrigger>
              </TabsList>
              <TabsContent value="build">
                <ConvertPanel
                  target={target}
                  onTarget={setTarget}
                  onConvert={handleConvert}
                  busy={busy}
                  hasMapping={!!job.mappingPath}
                  useLLM={useLLMConvert}
                  onUseLLM={setUseLLMConvert}
                  onGoMapping={() => setStep("mapping")}
                  converted={job.converted}
                  onOpenFile={openFile}
                  onSaveFile={handleSaveFile}
                  savingFile={savingFile}
                  selFile={selFile}
                  fileContent={fileContent}
                  loadingFile={loadingFile}
                  provenance={selProvenance}
                  onGoTrace={() => setConvertSub("trace")}
                  jobId={job.id}
                  noKey={llmStatus ? !llmStatus.anyKey : false}
                />
              </TabsContent>
              <TabsContent value="trace">
                <LineageExplorer
                  lineage={lineage}
                  loading={lineageLoading}
                  error={lineageError}
                  onOpenFile={openTraceFile}
                />
              </TabsContent>
              <TabsContent value="logs">
                <Card>
                  <CardContent className="p-0">
                    <ScrollArea className="max-h-[480px] border-0">
                      <pre className="whitespace-pre-wrap p-3 font-mono text-xs">
                        {logs.length ? logs.join("\n") : "(no log lines yet)"}
                      </pre>
                    </ScrollArea>
                    <div className="border-t p-2">
                      <Button size="sm" variant="ghost" onClick={() => navigator.clipboard.writeText(logs.join("\n"))}>
                        Copy logs
                      </Button>
                    </div>
                  </CardContent>
                </Card>
              </TabsContent>
            </Tabs>
            <div className="flex gap-2">
              <Button size="sm" variant="outline" onClick={() => setStep("mapping")}>
                <ArrowLeft className="h-3.5 w-3.5" /> Mapping
              </Button>
              {job.converted && (
                <Button size="sm" onClick={() => setStep("tests")}>
                  Next: Tests <ArrowRight className="h-3.5 w-3.5" />
                </Button>
              )}
            </div>
          </div>
        )}

        {job && step === "tests" && (
          <div className="space-y-3">
            <Tabs value={testsSub} onValueChange={(v) => setTestsSub(v as TestsSub)}>
              <TabsList className="flex h-auto max-w-full flex-wrap justify-start gap-1 overflow-x-auto">
                <TabsTrigger value="tests">
                  <FlaskConical className="h-3.5 w-3.5" /> Tests
                </TabsTrigger>
                <TabsTrigger value="metrics">
                  <Activity className="h-3.5 w-3.5" /> Metrics
                </TabsTrigger>
              </TabsList>
              <TabsContent value="tests">
                <TestsPanel
                  jobId={job.id}
                  busy={busy}
                  canRun={job.converted?.target === "go"}
                  gap={job.gentestGap}
                  testFiles={job.gentestFiles}
                  log={job.gentestLog}
                  preflight={job.gentestPreflight}
                  summary={job.gentestSummary}
                  hasSnapshot={!!job.gentestOutRoot}
                  llmKeyOk={llmStatus?.anyKey}
                  onGap={(opts) => handleGentest("check", opts)}
                  onGenerate={(opts) => handleGentest("generate", opts)}
                  onGoConvert={() => setStep("convert")}
                  onUploadLog={handleUploadLog}
                  onClearLog={handleClearLog}
                />
              </TabsContent>
              <TabsContent value="metrics">
                <MetricsPanel />
              </TabsContent>
            </Tabs>
            <div className="flex gap-2">
              <Button size="sm" variant="outline" onClick={() => setStep("convert")}>
                <ArrowLeft className="h-3.5 w-3.5" /> Convert
              </Button>
            </div>
          </div>
        )}

        <footer className="mt-8 border-t pt-4 text-[11px] text-muted-foreground">
          Batch flow — <span className="font-medium">Upload</span> (up to 50 files / .zip) →{" "}
          <span className="font-medium">Overview + Analyze</span> (triage LOW/MEDIUM/HIGH) →{" "}
          <span className="font-medium">Scenarios</span> → <span className="font-medium">Step 1 mapping</span> →{" "}
          <span className="font-medium">Step 2 convert</span> → <span className="font-medium">Tests</span>. Each step
          has its own LLM toggle (off = <code className="font-mono">-no-llm</code>, no keys needed; on = LLM seam
          when a key resolves). Converted trees live in disposable server tmpdirs — use{" "}
          <span className="font-medium">Download .zip</span> for the durable copy.
        </footer>
      </main>
    </div>
  );
}
