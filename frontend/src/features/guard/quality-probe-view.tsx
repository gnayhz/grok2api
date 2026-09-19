import { useQuery } from "@tanstack/react-query";
import { Activity, ArrowRight, CheckCircle2, ChevronRight, CircleHelp, Clock3, ExternalLink, LayoutGrid, Network, RefreshCw, Search, ShieldAlert, SlidersHorizontal, Table as TableIcon, UserRound } from "lucide-react";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "react-router-dom";
import { getAccountIdentities } from "@/entities/account/account-api";
import { listAllEgressNodes } from "@/entities/egress/egress-api";
import { fetchQualityNodes, fetchQualityProbes, fetchQualitySettings, type QualityProbeTask } from "@/entities/guard/quality-api";
import type { ResourceCheck, ResourceKind } from "@/entities/guard/resource-check-api";
import { ResourceCheckReport } from "@/entities/guard/resource-check-report";
import { Pagination } from "@/shared/components/pagination";
import { cn } from "@/shared/lib/cn";
import { useNow } from "@/shared/lib/use-now";
import { Badge } from "@/shared/ui/badge";
import { Button } from "@/shared/ui/button";
import { Dialog, DialogDescription, DialogHeader, DialogTitle } from "@/shared/ui/dialog";
import { Input } from "@/shared/ui/input";
import { OperationsDialogContent } from "@/shared/ui/operations";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/shared/ui/select";
import { Spinner } from "@/shared/ui/spinner";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/shared/ui/table";
import { LoadFailed } from "./quality-tribunal-view";
import { buildQualityExitIPIndex, filterProbeRecords, getProbeFinding, parseGoDurationMs, probeDurationMs, probeReferences, probeResult, probeSummary, type QualityAccountIdentity, type QualityNodeIdentity } from "./quality-view";

const PAGE_SIZE = 12;
type Directory = { accounts: Map<string, QualityAccountIdentity>; nodes: Map<string, QualityNodeIdentity> };
type Target = { kind: ResourceKind; id: string };
const targetKey = (target: Target) => `${target.kind}:${target.id}`;
function targets(task: QualityProbeTask): Target[] {
  const proofTargets = task.proof?.results?.map(p => ({ kind: p.kind, id: p.resource_id }));
  if (proofTargets?.length) return proofTargets;
  return [
    { kind: "account" as const, id: String(task.defendant) },
    { kind: "node" as const, id: String(task.node_id) },
  ].filter(target => target.id !== "0");
}
function targetName(target: Target, directory: Directory) {
  return (target.kind === "account" ? directory.accounts.get(target.id)?.name || directory.accounts.get(target.id)?.email : directory.nodes.get(target.id)?.name) || `#${target.id}`;
}
function tone(value: string) {
  if (value === "ok") return "border-emerald-500/25 bg-emerald-500/5 text-emerald-800 dark:text-emerald-300";
  if (value === "bad") return "border-rose-500/25 bg-rose-500/5 text-rose-800 dark:text-rose-300";
  if (value === "warn") return "border-amber-500/25 bg-amber-500/5 text-amber-800 dark:text-amber-300";
  return "border-border bg-muted/30 text-muted-foreground";
}
function FindingBadge({ task }: { task: QualityProbeTask }) {
  const { t } = useTranslation();
  const finding = getProbeFinding(task, t);
  return <Badge variant="outline" className={cn("shrink-0", tone(finding.tone))}>{finding.badge}</Badge>;
}
function ResourceList({ task, directory }: { task: QualityProbeTask; directory: Directory }) {
  const { t } = useTranslation();
  return <div className="space-y-2">{targets(task).map(target => {
    const proof = task.state === "done" ? task.proof?.results?.find(p => p.kind === target.kind && p.resource_id === target.id) : undefined;
    const name = targetName(target, directory);
    const Icon = target.kind === "account" ? UserRound : Network;
    return <div key={targetKey(target)} className="flex min-w-0 items-start gap-2 text-xs">
      <Icon className="mt-0.5 size-3.5 shrink-0 text-muted-foreground" />
      <div className="min-w-0 flex-1"><p className="truncate font-medium" title={name}>{name}</p><p className="mt-0.5 text-[11px] text-muted-foreground">{t(`guardProbes.${target.kind === "account" ? "accountLabel" : "exitLabel"}`)} · #{target.id}</p></div>
      {proof && <span className={cn("shrink-0 rounded-md border px-1.5 py-0.5 text-[11px]", tone(proof.outcome === "healthy" ? "ok" : proof.outcome === "degraded" ? "bad" : "warn"))}>{t(`guardProbes.${proof.outcome === "healthy" ? "targetNormal" : proof.outcome === "degraded" ? "targetAbnormal" : "targetUnknown"}`)}</span>}
    </div>;
  })}</div>;
}

function ResourceCell({ task, kind, directory }: { task: QualityProbeTask; kind: ResourceKind; directory: Directory }) {
  const { t } = useTranslation();
  const target = targets(task).find(item => item.kind === kind);
  if (!target) return <span className="text-xs text-muted-foreground">—</span>;
  const name = targetName(target, directory);
  const proof = task.state === "done" ? task.proof?.results?.find(item => item.kind === kind && item.resource_id === target.id) : undefined;
  const status = proof?.outcome === "healthy" ? "targetNormal" : proof?.outcome === "degraded" ? "targetAbnormal" : "targetUnknown";
  const color = proof?.outcome === "healthy" ? "text-emerald-700 dark:text-emerald-400" : proof?.outcome === "degraded" ? "text-rose-700 dark:text-rose-400" : "text-muted-foreground";
  return <div className="min-w-0 pr-4">
    <p className="truncate text-xs font-medium" title={name}>{name}</p>
    <p className="mt-1 flex items-center gap-1.5 text-[11px] text-muted-foreground">
      <span className="shrink-0">#{target.id}</span>
      {proof && <span className={cn("inline-flex min-w-0 items-center gap-1.5", color)}><span aria-hidden="true">·</span><span className="truncate">{t(`guardProbes.${status}`)}</span></span>}
    </p>
  </div>;
}

export function QualityProbeView() {
  const { t, i18n } = useTranslation();
  const [page, setPage] = useState(1);
  const [viewMode, setViewMode] = useState<"cards" | "table">("cards");
  const [search, setSearch] = useState("");
  const [result, setResult] = useState("all");
  const [selected, setSelected] = useState<QualityProbeTask | null>(null);
  const probesQuery = useQuery({ queryKey: ["quality", "probes"], queryFn: ({ signal }) => fetchQualityProbes(signal), refetchInterval: 15_000 });
  const settingsQuery = useQuery({ queryKey: ["quality", "settings"], queryFn: ({ signal }) => fetchQualitySettings(signal), staleTime: 60_000 });
  const probes = useMemo(() => probesQuery.data ?? [], [probesQuery.data]);
  const accountIDs = [...new Set(probes.flatMap(task => probeReferences(task).accounts))].sort();
  const accountsQuery = useQuery({ queryKey: ["accounts", "identities", accountIDs], queryFn: ({ signal }) => getAccountIdentities(accountIDs, signal), enabled: accountIDs.length > 0, staleTime: 10_000, refetchInterval: 10_000 });
  const namesQuery = useQuery({ queryKey: ["quality", "egress-names"], queryFn: ({ signal }) => listAllEgressNodes({}, signal), staleTime: 60_000, refetchInterval: 300_000 });
  const nodesQuery = useQuery({ queryKey: ["quality", "nodes"], queryFn: ({ signal }) => fetchQualityNodes(signal), staleTime: 30_000, refetchInterval: 30_000 });
  const directory = useMemo<Directory>(() => ({ accounts: new Map((accountsQuery.data?.items ?? []).map(a => [a.id, { name: a.name, email: a.email }])), nodes: new Map((namesQuery.data?.items ?? []).map(n => [n.id, { name: n.name }])) }), [accountsQuery.data, namesQuery.data]);
  const ips = useMemo(() => buildQualityExitIPIndex(nodesQuery.data ?? []), [nodesQuery.data]);
  const now = useNow(15_000);
  const windowMs = parseGoDurationMs(settingsQuery.data?.evidence_window) ?? 1_800_000;
  const summary = probeSummary(probes, now, windowMs);
  const filtered = useMemo(() => filterProbeRecords(probes, result, search, directory.accounts, directory.nodes, ips), [probes, result, search, directory, ips]);
  const currentPage = Math.min(page, Math.max(1, Math.ceil(filtered.length / PAGE_SIZE)));
  const rows = filtered.slice((currentPage - 1) * PAGE_SIZE, currentPage * PAGE_SIZE);
  // Keep the open report current while retaining it if the recent list rolls over.
  const inspected = probes.find(task => task.id === selected?.id) ?? selected;
  const formatter = useMemo(() => new Intl.DateTimeFormat(i18n.language, { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" }), [i18n.language]);
  const date = (value: string) => Number.isFinite(Date.parse(value)) ? formatter.format(new Date(value)) : "—";
  const duration = (task: QualityProbeTask) => { const ms = probeDurationMs(task); return ms === null ? "—" : `${(ms / 1000).toFixed(1)}s`; };
  const resultOptions = [["all", "resultAll"], ["degraded", "filterAbnormal"], ["clean", "filterNormal"], ["error", "filterUnresolved"], ["running", "stateRunning"], ["cancelled", "stateCancelled"]];
  const resetFilters = () => { setSearch(""); setResult("all"); setPage(1); };

  return <div className="space-y-5" aria-label={t("guardProbes.recordsLabel")}>
    <div className="grid grid-cols-2 gap-3 md:grid-cols-4">{[
      { label: "telemetryInFlight", value: summary.inFlight, icon: Activity, className: "text-blue-500", note: t("guardProbes.telemetryPending", { count: summary.pending }) },
      { label: "telemetryDegraded", value: summary.degraded, icon: ShieldAlert, className: "text-rose-500", note: t("guardProbes.telemetryDegradedHelp") },
      { label: "telemetryClean", value: summary.clean, icon: CheckCircle2, className: "text-emerald-500", note: t("guardProbes.telemetryCleanHelp") },
      { label: "telemetryErrors", value: summary.error + summary.cancelled, icon: CircleHelp, className: "text-amber-500", note: t("guardProbes.telemetryWindow", { window: Math.round(windowMs / 60_000) }) },
    ].map(item => <section key={item.label} className="min-w-0 rounded-xl border bg-card/60 p-4" aria-label={t(`guardProbes.${item.label}`)}><div className="flex items-center justify-between gap-2"><h3 className="text-xs font-medium text-muted-foreground">{t(`guardProbes.${item.label}`)}</h3><item.icon className={cn("size-4 shrink-0", item.className)} /></div><p className="my-2 text-2xl font-semibold tabular-nums">{item.value}</p><p className="text-[11px] leading-5 text-muted-foreground">{item.note}</p></section>)}</div>
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2 rounded-xl border bg-card/60 p-2.5">
        <div className="relative min-w-0 basis-full sm:basis-auto sm:flex-1">
          <Search className="pointer-events-none absolute left-3 top-3 size-4 text-muted-foreground" />
          <Input type="search" aria-label={t("guardProbes.searchPlaceholder")} placeholder={t("guardProbes.searchPlaceholder")}
            value={search} onChange={event => { setSearch(event.target.value); setPage(1); }} className="h-10 border-0 bg-transparent pl-9 shadow-none" />
        </div>
        <Select value={result} onValueChange={value => { setResult(value); setPage(1); }}>
          <SelectTrigger aria-label={t("guardProbes.resultFilter")} className="h-9 w-auto min-w-36 gap-2 bg-secondary/50">
            <SlidersHorizontal className="size-3.5 shrink-0 text-muted-foreground" /><SelectValue />
          </SelectTrigger>
          <SelectContent>{resultOptions.map(([value, key]) => <SelectItem key={value} value={value}>{t(`guardProbes.${key}`)}</SelectItem>)}</SelectContent>
        </Select>
        <div className="ml-auto flex shrink-0 items-center gap-2 sm:ml-1">
          <div className="flex rounded-lg bg-secondary/50 p-0.5" role="group" aria-label={t("guardProbes.viewMode")}>
            {(["cards", "table"] as const).map(mode => <Button key={mode} variant="ghost" size="icon"
              className={cn("size-8 rounded-md text-muted-foreground", viewMode === mode && "bg-background text-foreground shadow-sm")}
              aria-label={t(`guardProbes.${mode === "cards" ? "viewCards" : "viewTable"}`)} title={t(`guardProbes.${mode === "cards" ? "viewCards" : "viewTable"}`)}
              aria-pressed={viewMode === mode} onClick={() => setViewMode(mode)}>
              {mode === "cards" ? <LayoutGrid className="size-4" /> : <TableIcon className="size-4" />}
            </Button>)}
          </div>
          <Button variant="ghost" size="icon" className="size-9 shrink-0 text-muted-foreground" aria-label={t("common.refresh")} title={t("common.refresh")}
            disabled={probesQuery.isFetching} onClick={() => void probesQuery.refetch()}>
            <RefreshCw className={cn("size-4", probesQuery.isFetching && "animate-spin")} />
          </Button>
        </div>
      </div>
      <div className="flex flex-wrap items-center justify-between gap-2 px-1 text-xs text-muted-foreground">
        <div className="flex items-center gap-2">
          <span>{t("guardProbes.totalRecords", { count: probes.length })}{filtered.length !== probes.length && ` · ${t("guardProbes.filteredRecords", { count: filtered.length })}`}</span>
          {(search || result !== "all") && <button type="button" className="text-primary hover:underline" onClick={resetFilters}>{t("guardProbes.clearFilters")}</button>}
        </div>
        <span>{t("guardProbes.recordsHelp")}</span>
      </div>
    </div>
    {(accountsQuery.isError || namesQuery.isError) && <div role="alert" className="flex items-center gap-2 text-xs text-muted-foreground">{t("guardProbes.namesUnavailable")}<Button size="sm" variant="ghost" onClick={() => { void accountsQuery.refetch(); void namesQuery.refetch(); }}>{t("common.retry")}</Button></div>}
    {probesQuery.isError ? <LoadFailed onRetry={() => void probesQuery.refetch()} /> : probesQuery.isLoading ? <div role="status" className="flex min-h-40 items-center justify-center gap-2"><Spinner />{t("common.loading")}</div> : !rows.length ? <div className="rounded-xl border border-dashed p-10 text-center"><p className="text-sm font-medium">{t("guardProbes.emptyTitle")}</p><p className="mt-2 text-xs text-muted-foreground">{t("guardProbes.emptyDesc")}</p></div> : viewMode === "cards" ?
      <div className="grid gap-3.5 lg:grid-cols-2">{rows.map(task => {
        const finding = getProbeFinding(task, t);
        return <article key={task.id} aria-label={t("guardProbes.recordLabel", { id: task.id })} className="flex min-w-0 flex-col rounded-xl border bg-card/60 p-4">
          <header className="flex flex-wrap items-center justify-between gap-2 border-b pb-3"><div className="flex flex-wrap items-center gap-2"><span className="text-xs font-semibold">#{task.id}</span><Link className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-primary hover:underline" to={`/guard?case=${task.case_id}#tribunal`}>{t("guardProbes.modalCase", { caseId: task.case_id })}<ExternalLink className="size-3" /></Link></div><FindingBadge task={task} /></header>
          <div className="my-3 rounded-lg border bg-muted/20 p-3"><ResourceList task={task} directory={directory} /></div>
          <p className={cn("rounded-lg border p-3 text-xs leading-5", tone(finding.tone))}>{finding.text}</p>
          {task.proof && <p className="mt-3 text-xs text-muted-foreground">{t("guardProbes.proofProgress", { calls: task.proof.calls, max: task.proof.max_calls, generations: task.proof.generations ?? 0 })}</p>}
          <footer className="mt-auto flex flex-wrap items-center justify-between gap-2 pt-4 text-xs text-muted-foreground"><span className="flex items-center gap-1.5"><Clock3 className="size-3.5" />{date(task.created_at)} · {duration(task)}</span><Button size="sm" variant="ghost" className="h-7 gap-1 text-xs text-primary" onClick={() => setSelected(task)}>{t("guardProbes.viewReportBtn")}<ArrowRight className="size-3.5" /></Button></footer>
        </article>;
      })}</div> :
      <div className="overflow-hidden rounded-xl border bg-card/40">
        <Table className="min-w-[900px] table-fixed">
          <TableHeader className="bg-muted/30"><TableRow className="hover:bg-transparent">
            <TableHead className="w-28 pl-4">{t("guardProbes.recordColumn")}</TableHead>
            <TableHead className="w-[27%]">{t("guardProbes.accountLabel")}</TableHead>
            <TableHead className="w-[23%]">{t("guardProbes.exitLabel")}</TableHead>
            <TableHead className="w-36">{t("guardProbes.findingColumn")}</TableHead>
            <TableHead className="w-32">{t("guardProbes.createdAtLabel")}</TableHead>
            <TableHead className="w-12 pr-4"><span className="sr-only">{t("guardProbes.reportColumn")}</span></TableHead>
          </TableRow></TableHeader>
          <TableBody>{rows.map(task => <TableRow key={task.id} className="group">
            <TableCell className="py-3.5 pl-4 text-xs">
              <button type="button" className="font-semibold tabular-nums hover:text-primary hover:underline" aria-label={t("guardProbes.openReport", { id: task.id })} onClick={() => setSelected(task)}>#{task.id}</button>
              <Link className="mt-1 block truncate text-[11px] text-muted-foreground hover:text-primary hover:underline" to={`/guard?case=${task.case_id}#tribunal`}>{t("guardProbes.modalCase", { caseId: task.case_id })}</Link>
            </TableCell>
            <TableCell><ResourceCell task={task} kind="account" directory={directory} /></TableCell>
            <TableCell><ResourceCell task={task} kind="node" directory={directory} /></TableCell>
            <TableCell><FindingBadge task={task} />{task.proof && <p className="mt-1 text-[11px] text-muted-foreground">{t("guardProbes.measurementCount", { count: task.proof.calls })}</p>}</TableCell>
            <TableCell className="text-xs tabular-nums text-muted-foreground"><p>{date(task.created_at)}</p><p className="mt-1 text-[11px]">{t("guardProbes.elapsed", { duration: duration(task) })}</p></TableCell>
            <TableCell className="pr-3"><Button variant="ghost" size="icon" className="size-8 text-muted-foreground group-hover:text-foreground"
              aria-label={t("guardProbes.openReport", { id: task.id })} title={t("guardProbes.viewReportBtn")} onClick={() => setSelected(task)}><ChevronRight className="size-4" /></Button></TableCell>
          </TableRow>)}</TableBody>
        </Table>
      </div>}

    {filtered.length > PAGE_SIZE && <Pagination page={currentPage} pageSize={PAGE_SIZE} total={filtered.length} onPageChange={setPage} />}
    {inspected && <ProbeReport key={inspected.id} task={inspected} directory={directory} now={now} onClose={() => setSelected(null)} />}
  </div>;
}

function ProbeReport({ task, directory, now, onClose }: { task: QualityProbeTask; directory: Directory; now: number; onClose: () => void }) {
  const { t } = useTranslation();
  const [selection, setSelection] = useState("");
  const items = targets(task);
  const target = items.find(item => targetKey(item) === selection) ?? items[0];
  const state = (["pending", "running", "done", "failed", "cancelled"].includes(task.state) ? task.state : "failed") as ResourceCheck["state"];
  const check: ResourceCheck | undefined = target ? { id: String(task.id), kind: target.kind, resource_id: target.id, model: task.experiment?.baseline.model || "", state, created_at: task.created_at, finished_at: task.finished_at ?? undefined, report: task.proof } : undefined;
  return <Dialog open onOpenChange={open => { if (!open) onClose(); }}><OperationsDialogContent className="max-h-[90dvh] max-w-4xl overflow-y-auto">
    <DialogHeader><DialogTitle className="pr-6 text-base">{t("guardProbes.modalTitle", { id: task.id })}</DialogTitle><DialogDescription>{t("guardProbes.caseResultNote")}</DialogDescription><Link className="inline-flex items-center gap-1 text-xs text-primary hover:underline" to={`/guard?case=${task.case_id}#tribunal`} onClick={onClose}>{t("guardProbes.actionGoCase")}<ExternalLink className="size-3" /></Link></DialogHeader>
    <>
      {items.length > 1 && <div className="flex flex-wrap gap-2" role="group" aria-label={t("guardProbes.resourcesColumn")}>{items.map(item => <Button key={targetKey(item)} variant={targetKey(item) === (target && targetKey(target)) ? "secondary" : "outline"} size="sm" className="max-w-full" aria-pressed={targetKey(item) === (target && targetKey(target))} onClick={() => setSelection(targetKey(item))}><span className="truncate">{t(`guardProbes.${item.kind === "account" ? "accountLabel" : "exitLabel"}`)} · {targetName(item, directory)}</span></Button>)}</div>}
      {check && target ? <ResourceCheckReport key={targetKey(target)} check={check} target={{ id: target.id, name: targetName(target, directory) }} now={now} /> : <p className="py-5 text-sm text-muted-foreground">{t(`guardProbes.${probeResult(task) === "running" ? "proofWaiting" : "proofInconclusive"}`)}</p>}
    </>
  </OperationsDialogContent></Dialog>;
}
