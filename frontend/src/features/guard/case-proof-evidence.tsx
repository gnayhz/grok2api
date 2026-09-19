import { useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { ArrowDown, ArrowRight, Check, CircleHelp, Clock3, LoaderCircle, Network, ShieldAlert, ShieldCheck, UserRound } from "lucide-react";
import type { QualityCase, QualityProbeTask } from "@/entities/guard/quality-api";
import type { ResourceObservation } from "@/entities/guard/resource-check-api";
import { cn } from "@/shared/lib/cn";
import { Badge } from "@/shared/ui/badge";
import { Button } from "@/shared/ui/button";
import { caseReportEn, caseReportZh } from "./case-report-copy";
import { caseReportEvidence, caseReportFinding, caseReportReason } from "./case-report-presentation";
import type { QualityAccountIdentity, QualityNodeIdentity } from "./quality-view";

const tones = {
 healthy: "border-emerald-500/25 bg-emerald-500/5 text-emerald-800 dark:text-emerald-300",
 degraded: "border-rose-500/25 bg-rose-500/5 text-rose-800 dark:text-rose-300",
 inconclusive: "border-amber-500/25 bg-amber-500/5 text-amber-800 dark:text-amber-300",
 pending: "border-border bg-muted/30 text-muted-foreground",
};
function tone(outcome: string) { return tones[outcome as keyof typeof tones] ?? tones.inconclusive; }

export function CaseProofEvidence({ item, tasks, loading, error, retry, accounts, nodes }: {
 item: QualityCase; tasks: QualityProbeTask[]; loading: boolean; error: boolean; retry: () => void;
 accounts: Map<number, QualityAccountIdentity>; nodes: Map<number, QualityNodeIdentity>;
}) {
 const { i18n } = useTranslation();
 const copy = i18n.language.startsWith("zh") ? caseReportZh : caseReportEn;
 const [highlight, setHighlight] = useState<number>();
 const steps = useRef(new Map<number, HTMLLIElement>());
 const task = tasks.find(row => row.direction === "case_proof");
 const report = item.proof ?? task?.proof;
 const finding = caseReportFinding(item, report) as keyof typeof copy.findings;
 const running = item.status === "investigating";
 const observations = report?.observations ?? [];
 const bad = ["account", "exit", "both"].includes(finding);
 const passed = finding === "normal";
 const FindingIcon = running ? LoaderCircle : bad ? ShieldAlert : passed ? ShieldCheck : CircleHelp;
 const accountID = String(item.parties.find(p => p.kind === "account")?.account_id ?? "");
 const exitID = String(item.parties.find(p => p.kind === "exit")?.node_id ?? "");
 const accountName = (id: string) => accounts.get(Number(id))?.name || accounts.get(Number(id))?.email || `${copy.account} #${id}`;
 const exitName = (id: string) => nodes.get(Number(id))?.name || `${copy.exit} #${id}`;
 const showStep = (id: number) => {
  setHighlight(id);
  const element = steps.current.get(id);
  element?.scrollIntoView({ behavior: "auto", block: "nearest" });
  element?.focus({ preventScroll: true });
 };
 const duration = item.closed_at ? Math.max(0, Math.round((Date.parse(item.closed_at) - Date.parse(item.opened_at)) / 1000)) : null;
 const model = task?.experiment?.baseline.model || observations.find(o => o.sample.attempt.model)?.sample.attempt.model;
 const statusLabel = (outcome: string) => copy.outcomes[outcome as keyof typeof copy.outcomes] ?? copy.outcomes.inconclusive;
 const observationLabel = (value: string) => copy.observations[value as keyof typeof copy.observations] ?? copy.observations.unknown;
 const stepTitle = (o: ResourceObservation, index: number) => {
  const previous = observations[index - 1];
  if (previous?.sample.path_verified && o.sample.path_verified && previous.sample.path_key && previous.sample.path_key === o.sample.path_key && previous.sample.path_family === o.sample.path_family && previous.account_id !== o.account_id) return copy.changeAccount;
  if (previous && previous.account_id === o.account_id && previous.node_id !== o.node_id) return copy.changeExit;
  return copy.stepPurpose[o.purpose as keyof typeof copy.stepPurpose] ?? copy.record;
 };
 const stepNote = (o: ResourceObservation) => ({ A: copy.normalNote, B: copy.badNote, conflict: copy.conflictNote, pending: copy.pendingNote }[o.class] ?? copy.unknownNote);

 return <div className="space-y-6 text-sm">
  <section className={cn("overflow-hidden rounded-xl border", tone(bad ? "degraded" : passed ? "healthy" : running ? "pending" : "inconclusive"))} aria-label={copy.eyebrow}>
   <div className="flex gap-3 p-4 sm:p-5">
    <div className="flex size-10 shrink-0 items-center justify-center rounded-full bg-background/75"><FindingIcon className={cn("size-5", running && "animate-spin")} /></div>
    <div className="min-w-0"><p className="mb-1 text-xs font-medium opacity-75">{copy.eyebrow}</p><h3 className="text-lg font-semibold tracking-tight">{copy.findings[finding]}</h3><p className="mt-1.5 text-sm leading-relaxed opacity-90">{copy.summaries[finding]}</p></div>
   </div>
   <dl className="grid grid-cols-2 gap-px border-t border-current/10 bg-current/5 sm:grid-cols-4">
    {[
     [copy.calls, report ? `${report.calls} ${copy.attempt}` : copy.dash],
     [copy.generations, report?.generations !== undefined ? `${report.generations} ${copy.attempt}` : copy.dash],
     [copy.duration, duration === null ? (running ? copy.pending : copy.dash) : duration >= 60 ? `${Math.floor(duration / 60)} ${copy.minutes} ${duration % 60} ${copy.seconds}` : `${duration} ${copy.seconds}`],
     [copy.model, model || copy.dash],
    ].map(([label, value]) => <div key={label} className="min-w-0 bg-background/70 px-4 py-3"><dt className="text-xs text-muted-foreground">{label}</dt><dd className="mt-1 truncate font-semibold text-foreground" title={value}>{value}</dd></div>)}
   </dl>
  </section>

  <div className="grid items-start gap-6 lg:grid-cols-[minmax(0,1.15fr)_minmax(0,1fr)]">
  <div className="min-w-0 space-y-5 lg:order-2">
  <section aria-label={copy.overview} className="space-y-3">
   <h3 className="font-semibold">{copy.overview}</h3>
   <div className="grid gap-3">{item.parties.map(p => {
    const id = String(p.kind === "account" ? p.account_id : p.node_id);
    const proof = report?.results?.find(r => r.kind === (p.kind === "account" ? "account" : "node") && String(r.resource_id) === id);
    const outcome = proof?.outcome ?? (running ? "pending" : "inconclusive");
    const Icon = p.kind === "account" ? UserRound : Network;
    const title = p.kind === "account" ? accountName(id) : exitName(id);
    return <div key={`${p.kind}:${id}`} className="min-w-0 rounded-xl border bg-card p-4">
     <div className="mb-3 flex items-center justify-between gap-2"><span className="flex items-center gap-1.5 text-xs text-muted-foreground"><Icon className="size-3.5" />{p.kind === "account" ? copy.targetAccount : copy.targetExit}</span><Badge variant="outline" className={cn("shrink-0", tone(outcome))}>{statusLabel(outcome)}</Badge></div>
     <p className="truncate font-semibold" title={title}>{title}</p><p className="mt-1 text-xs text-muted-foreground">#{id}</p>
     <div className="mt-3 flex items-center gap-1.5 border-t pt-3 text-xs"><span className={cn("size-1.5 shrink-0 rounded-full", ["sentenced", "remanded"].includes(p.disposition) ? "bg-rose-500" : "bg-slate-400")} />{copy.dispositions[p.disposition as keyof typeof copy.dispositions] ?? copy.dispositions.unknown}</div>
    </div>;
   })}</div>
   <p className="text-xs leading-relaxed text-muted-foreground">{copy.heldNote}</p>
  </section>

  {report?.results?.length ? <section className="space-y-3" aria-label={copy.basis}>
   <div><h3 className="font-semibold">{copy.basis}</h3><p className="mt-1 text-xs text-muted-foreground">{copy.basisHelp}</p></div>
   {report.results.map(proof => {
    const evidence = caseReportEvidence(proof, report);
    const label = proof.kind === "account" ? accountName(String(proof.resource_id)) : exitName(String(proof.resource_id));
    return <div key={`${proof.kind}:${proof.resource_id}`} className="rounded-xl border bg-muted/15 p-4">
     <div className="mb-2 flex items-start gap-2"><CircleHelp className={cn("mt-0.5 size-4 shrink-0", proof.outcome === "inconclusive" ? "text-amber-600" : "text-primary")} /><p className="min-w-0 font-medium">{proof.kind === "account" ? copy.account : copy.exit} · {label}</p></div>
     <p className="text-sm leading-6 text-muted-foreground">{copy.reasons[caseReportReason(proof) as keyof typeof copy.reasons]}</p>
     {evidence.length > 0 && <div className="mt-3 flex flex-wrap items-center gap-2 text-xs"><span className="text-muted-foreground">{copy.evidence}</span>{evidence.map(o => <button key={o.id} type="button" onClick={() => showStep(o.id)} className="rounded-md border bg-background px-2 py-1 font-medium hover:bg-accent focus-visible:outline-2 focus-visible:outline-ring">{copy.step} {o.id} <ArrowDown className="ml-1 inline size-3" /></button>)}</div>}
    </div>;
   })}
  </section> : null}

  </div>
  <section className="min-w-0 space-y-4 lg:order-1" aria-label={copy.process}>
   <div className="flex flex-wrap items-start justify-between gap-2"><div><h3 className="font-semibold">{copy.process}</h3><p className="mt-1 text-xs leading-relaxed text-muted-foreground">{copy.processHelp}</p></div>{report && <span className="text-xs text-muted-foreground">{copy.max} {report.max_calls} {copy.attempt}</span>}</div>
   {loading && <p role="status" className="flex items-center gap-2 py-3 text-muted-foreground"><LoaderCircle className="size-4 animate-spin" />{copy.fetching}</p>}
   {error && <div role="alert" className="flex flex-wrap items-center gap-2 rounded-lg border border-amber-500/30 p-3 text-amber-700 dark:text-amber-300">{copy.loadError}<Button size="sm" variant="outline" onClick={retry}>{copy.retry}</Button></div>}
   {!loading && !error && observations.length === 0 && <p className="rounded-xl border border-dashed p-5 text-center text-muted-foreground">{running ? copy.waiting : copy.empty}</p>}
   <ol className="space-y-3">{observations.map((o, index) => <li key={o.id} ref={element => { if (element) steps.current.set(o.id, element); else steps.current.delete(o.id); }} tabIndex={-1} className={cn("relative rounded-xl border bg-card p-4 outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring", highlight === o.id && "border-primary/50 bg-primary/[0.03]")}>
    <div className="flex flex-wrap items-center justify-between gap-2"><div className="flex items-center gap-2"><span className="flex size-6 items-center justify-center rounded-full bg-muted text-xs font-semibold">{o.id}</span><h4 className="text-sm font-medium">{stepTitle(o, index)}</h4></div><Badge variant="outline" className={tone(o.class === "A" ? "healthy" : o.class === "B" ? "degraded" : o.class === "pending" ? "pending" : "inconclusive")}>{observationLabel(o.class)}</Badge></div>
    <div className="my-3 grid min-w-0 grid-cols-[1fr_auto_1fr] items-center gap-2 rounded-lg bg-muted/35 p-3">
     <div className="min-w-0"><p className="mb-1 flex items-center gap-1 text-[11px] text-muted-foreground"><UserRound className="size-3 shrink-0" />{String(o.account_id) === accountID ? copy.targetAccount : copy.controlAccount}</p><p className="break-words text-xs font-medium">{accountName(String(o.account_id))}</p></div>
     <ArrowRight aria-label={copy.flow} className="size-4 text-muted-foreground" />
     <div className="min-w-0"><p className="mb-1 flex items-center gap-1 text-[11px] text-muted-foreground"><Network className="size-3 shrink-0" />{String(o.node_id) === exitID ? copy.targetExit : copy.controlExit}</p><p className="break-words text-xs font-medium">{exitName(String(o.node_id))}</p></div>
    </div>
    <p className="text-xs leading-5 text-muted-foreground">{stepNote(o)}</p>
    <div className="mt-3 flex flex-wrap gap-x-4 gap-y-1 text-[11px] text-muted-foreground">{[[o.sample.completed, copy.checks.complete, copy.checks.incomplete], [o.sample.path_verified, copy.checks.path, copy.checks.unverified]].map(([ok, yes, no], index) => <span key={index} className="inline-flex items-center gap-1">{ok ? <Check className="size-3 text-emerald-600 dark:text-emerald-400" /> : <Clock3 className="size-3" />}{ok ? yes : no}</span>)}</div>
   </li>)}</ol>
  </section>
  </div>
  <p className="border-t pt-4 text-xs leading-relaxed text-muted-foreground">{copy.history}</p>
 </div>;
}
