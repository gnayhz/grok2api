import { useRef, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { ArrowDown, ArrowRight, Check, CircleHelp, Clock3, LoaderCircle, Network, ShieldAlert, ShieldCheck, UserRound } from "lucide-react";
import { getAccountIdentities } from "@/entities/account/account-api";
import { listAllEgressNodes } from "@/entities/egress/egress-api";
import { cn } from "@/shared/lib/cn";
import { Badge } from "@/shared/ui/badge";
import { Button } from "@/shared/ui/button";
import type { ResourceCheck, ResourceSample, ResourceTarget } from "./resource-check-api";
import { resourceCheckEn, resourceCheckZh } from "./resource-check-copy";
import { resourceEvidence, resourceFinding, resourceNormalExpired, resourceObservations, resourceProof } from "./resource-check-presentation";

function tone(value: string) {
  if (value === "healthy") return "border-emerald-500/25 bg-emerald-500/5 text-emerald-800 dark:text-emerald-300";
  if (value === "degraded") return "border-rose-500/25 bg-rose-500/5 text-rose-800 dark:text-rose-300";
  if (["inconclusive", "failed", "cancelled"].includes(value)) return "border-amber-500/25 bg-amber-500/5 text-amber-800 dark:text-amber-300";
  return "border-border bg-muted/30 text-muted-foreground";
}

export function ResourceFindingBadge({ check, now }: { check?: ResourceCheck; now: number }) {
  const { i18n } = useTranslation();
  const copy = i18n.language.startsWith("zh") ? resourceCheckZh : resourceCheckEn;
  const finding = resourceFinding(check);
  const expired = resourceNormalExpired(check, now);
  return <Badge variant="outline" className={cn("shrink-0", tone(expired ? "historical" : finding))}>{expired ? copy.historicalNormal : copy.outcomes[finding]}</Badge>;
}

export function ResourceCheckReport({ check, target, now }: { check: ResourceCheck; target: ResourceTarget; now: number }) {
  const { t, i18n } = useTranslation();
  const copy = i18n.language.startsWith("zh") ? resourceCheckZh : resourceCheckEn;
  const [showAll, setShowAll] = useState(false);
  const [highlight, setHighlight] = useState<string>();
  const steps = useRef(new Map<string, HTMLLIElement>());
  const report = check.report;
  const observations = report?.observations ?? [];
  const related = resourceObservations(check);
  const displayed = showAll ? observations : related;
  const evidence = resourceEvidence(check);
  const proof = resourceProof(check);
  const finding = resourceFinding(check);
  const running = finding === "pending" || finding === "running";
  const expired = resourceNormalExpired(check, now);
  const Icon = running ? LoaderCircle : finding === "degraded" ? ShieldAlert : finding === "healthy" ? ShieldCheck : CircleHelp;
  const reason = (code?: string) => t(`resourceChecks.reason.${code || "measurement_unavailable"}`, { defaultValue: t("resourceChecks.reason.measurement_unavailable") });
  const accountIDs = [...new Set([...observations.map(o => o.account_id), ...(report?.groups.flatMap(g => [g.account_id, g.control_account]) ?? [])])].filter(id => id && id !== "0").sort();
  const accounts = useQuery({ queryKey: ["accounts", "identities", accountIDs.join(",")], queryFn: ({ signal }) => getAccountIdentities(accountIDs, signal), enabled: accountIDs.length > 0, staleTime: 30000 });
  const nodes = useQuery({ queryKey: ["egress-nodes", "operations-summary"], queryFn: ({ signal }) => listAllEgressNodes({}, signal), enabled: observations.length > 0 || !!report?.groups.length, staleTime: 15000 });
  const accountName = (id: string) => check.kind === "account" && id === target.id ? target.name : accounts.data?.items.find(a => a.id === id)?.name || `${copy.account} #${id}`;
  const nodeName = (id: string) => check.kind === "node" && id === target.id ? target.name : nodes.data?.items.find(n => n.id === id)?.name || `${copy.node} #${id}`;
  const duration = check.finished_at ? Math.max(0, Math.round((Date.parse(check.finished_at) - Date.parse(check.created_at)) / 1000)) : null;
  const findingText = running ? copy.waiting : check.state !== "done" ? copy.stoppedNote : reason(proof?.reason || report?.reason);
  const label = (value: string) => copy.observations[value as keyof typeof copy.observations] ?? copy.observations.unknown;
  const note = (value: string) => ({ A: copy.normalNote, B: copy.badNote, conflict: copy.conflictNote, pending: copy.pendingNote }[value] ?? copy.unknownNote);
  const showStep = (key: string) => {
    setHighlight(key);
    const element = steps.current.get(key);
    element?.scrollIntoView({ behavior: "auto", block: "nearest" });
    element?.focus({ preventScroll: true });
  };
  return <div className="min-w-0 space-y-5 text-sm">
    <section className={cn("overflow-hidden rounded-xl border", tone(expired ? "historical" : finding))} aria-label={copy.result}>
      <div className="flex gap-3 p-4 sm:p-5"><div className="flex size-10 shrink-0 items-center justify-center rounded-full bg-background/75"><Icon className={cn("size-5", running && "animate-spin")} /></div>
        <div className="min-w-0"><p className="mb-1 text-xs opacity-75">{copy.result}</p><h3 aria-live="polite" className="text-lg font-semibold">{expired ? copy.historicalNormal : copy.outcomes[finding]}</h3><p className="mt-1.5 leading-relaxed">{findingText}</p>{expired && <p className="mt-2 text-xs leading-5">{t("resourceChecks.expired")}</p>}</div>
      </div>
      <dl className="grid grid-cols-2 gap-px border-t border-current/10 bg-current/5 sm:grid-cols-4">
        {[[copy.batchCalls, report ? `${report.calls} ${copy.times}` : "—"], [copy.batchGenerations, report?.generations !== undefined ? `${report.generations} ${copy.times}` : "—"], [copy.duration, duration === null ? "—" : duration >= 60 ? `${Math.floor(duration / 60)} ${copy.minutes} ${duration % 60} ${copy.seconds}` : `${duration} ${copy.seconds}`], [copy.model, check.model]].map(([title, value]) => <div key={title} className="min-w-0 bg-background/75 px-4 py-3"><dt className="text-xs text-muted-foreground">{title}</dt><dd className="mt-1 truncate font-semibold text-foreground" title={value}>{value}</dd></div>)}
      </dl>
    </section>
    {report && <p className="text-xs leading-5 text-muted-foreground">{copy.batchNote}</p>}
    {report?.version === "resource-quality-check-v1" && <p className="rounded-lg border border-amber-500/25 bg-amber-500/5 p-3 text-xs leading-5">{copy.historical}</p>}
    {check.state === "done" && evidence.length > 0 && <section className="rounded-xl border bg-muted/15 p-4" aria-label={copy.basis}>
      <h3 className="flex items-center gap-2 font-semibold"><CircleHelp className="size-4 text-muted-foreground" />{copy.basis}</h3><p className="mt-2 leading-6 text-muted-foreground">{findingText}</p>
      <div className="mt-3 flex flex-wrap items-center gap-2 text-xs"><span className="text-muted-foreground">{copy.evidence}</span>{evidence.map(o => <button key={`${o.window}:${o.id}`} type="button" onClick={() => showStep(`${o.window}:${o.id}`)} className="rounded-md border bg-background px-2 py-1 font-medium hover:bg-accent focus-visible:outline-2 focus-visible:outline-ring">{copy.step} {o.id}<ArrowDown className="ml-1 inline size-3" /></button>)}</div>
    </section>}
    {(accounts.isError || nodes.isError) && <div role="alert" className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">{copy.nameError}<Button variant="outline" size="sm" onClick={() => { if (accounts.isError) void accounts.refetch(); if (nodes.isError) void nodes.refetch(); }}>{copy.retry}</Button></div>}
    <section className="space-y-3" aria-label={copy.process}>
      <div className="flex flex-wrap items-start justify-between gap-2"><div><h3 className="font-semibold">{copy.process}</h3><p className="mt-1 text-xs leading-5 text-muted-foreground">{copy.flowNote}</p></div>{observations.length > related.length && <Button variant="outline" size="sm" onClick={() => setShowAll(value => !value)}>{showAll ? copy.showRelevant : copy.all}</Button>}</div>
      {observations.length > related.length && <p className="text-xs text-muted-foreground">{showAll ? copy.all : copy.relevant} · {displayed.length} / {observations.length}</p>}
      {!displayed.length && !report?.groups.length && <p role="status" className="rounded-xl border border-dashed p-5 text-center text-muted-foreground">{running ? copy.waiting : copy.noEvidence}</p>}
      <ol className="space-y-3">{displayed.map(o => {
        const previous = observations[observations.indexOf(o) - 1];
        const comparable = previous?.window === o.window && previous.sample.path_verified && o.sample.path_verified && !!previous.sample.path_key && !!o.sample.path_key && previous.sample.path_family === o.sample.path_family;
        const title = comparable && previous.sample.path_key === o.sample.path_key && previous.account_id !== o.account_id ? copy.changeAccount : comparable && previous.account_id === o.account_id && previous.sample.path_key !== o.sample.path_key ? copy.changeExit : o.purpose === "measure_target" ? copy.targetStep : copy.comparisonStep;
        const key = `${o.window}:${o.id}`;
        return <li key={key} ref={element => { if (element) steps.current.set(key, element); else steps.current.delete(key); }} tabIndex={-1} className={cn("rounded-xl border bg-card p-4 outline-none focus-visible:ring-2 focus-visible:ring-ring", highlight === key && "border-primary/50 bg-primary/[0.03]")}>
          <div className="flex flex-wrap items-center justify-between gap-2"><div className="flex items-center gap-2"><span className="flex size-6 shrink-0 items-center justify-center rounded-full bg-muted text-xs font-semibold">{o.id}</span><h4 className="font-medium">{title}</h4></div><Badge variant="outline" className={tone(o.class === "A" ? "healthy" : o.class === "B" ? "degraded" : o.class === "pending" ? "pending" : "inconclusive")}>{label(o.class)}</Badge></div>
          <div className="my-3 grid grid-cols-[minmax(0,1fr)_auto_minmax(0,1fr)] items-center gap-2 rounded-lg bg-muted/35 p-3">
            <div className="min-w-0"><p className="mb-1 flex items-center gap-1 text-[11px] text-muted-foreground"><UserRound className="size-3 shrink-0" />{copy.account}{check.kind === "account" && o.account_id === target.id && ` · ${copy.target}`}</p><p className="break-words text-xs font-medium">{accountName(o.account_id)}</p><p className="mt-1 break-all text-[10px] text-muted-foreground">#{o.account_id}</p></div>
            <ArrowRight aria-label={copy.via} className="size-4 text-muted-foreground" />
            <div className="min-w-0"><p className="mb-1 flex items-center gap-1 text-[11px] text-muted-foreground"><Network className="size-3 shrink-0" />{copy.node}{check.kind === "node" && o.node_id === target.id && ` · ${copy.target}`}</p><p className="break-words text-xs font-medium">{nodeName(o.node_id)}</p><p className="mt-1 break-all text-[10px] text-muted-foreground">#{o.node_id}</p></div>
          </div>
          <p className="text-xs leading-5 text-muted-foreground">{note(o.class)}</p><SampleFacts sample={o.sample} />
        </li>;
      })}</ol>
      {report?.groups.map((group, index) => <details key={index} className="rounded-xl border bg-card p-4"><summary className="cursor-pointer font-medium">{copy.legacyGroup} {index + 1}</summary><p className="mt-3 text-xs leading-5 text-muted-foreground">{reason(group.reason)}</p><div className="mt-3 space-y-3">
        {[...group.control.map(sample => ({ sample, title: copy.legacyControl, account: group.control_account, node: group.control_node })), ...group.samples.map(sample => ({ sample, title: copy.legacyTarget, account: group.account_id, node: group.node_id })), ...(group.after ? [{ sample: group.after, title: copy.legacyAfter, account: group.control_account, node: group.control_node }] : [])].map(({ sample, title, account, node }, i) => <div key={i} className="rounded-lg bg-muted/30 p-3"><div className="flex flex-wrap justify-between gap-2 text-xs"><span>{title}</span><span>{!sample.completed ? copy.incomplete : sample.thinking ? copy.observations.A : copy.observations.B}</span></div><p className="mt-2 break-words text-xs">{accountName(account)} {copy.via} {nodeName(node)}</p><SampleFacts sample={sample} /></div>)}
      </div></details>)}
    </section>
  </div>;
}

function SampleFacts({ sample }: { sample: ResourceSample }) {
  const { i18n } = useTranslation();
  const copy = i18n.language.startsWith("zh") ? resourceCheckZh : resourceCheckEn;
  return <div className="mt-3 flex flex-wrap gap-x-4 gap-y-1 text-[11px] text-muted-foreground">{[[sample.completed, copy.complete, copy.incomplete], [sample.path_verified, copy.path, copy.unverified]].map(([ok, yes, no], index) => <span key={index} className="inline-flex items-center gap-1">{ok ? <Check className="size-3 text-emerald-600 dark:text-emerald-400" /> : <Clock3 className="size-3" />}{ok ? yes : no}</span>)}</div>;
}
