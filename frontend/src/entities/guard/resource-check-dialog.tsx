import { useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { ChevronRight, CircleHelp, Network, Search, UserRound } from "lucide-react";
import { listModels } from "@/entities/model/model-api";
import { useLifetimeMutation } from "@/shared/hooks/use-lifetime-mutation";
import { showErrorToast } from "@/shared/lib/show-error";
import { cn } from "@/shared/lib/cn";
import { Button } from "@/shared/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/shared/ui/dialog";
import { Spinner } from "@/shared/ui/spinner";
import { Label } from "@/shared/ui/label";
import { ErrorState } from "@/shared/components/data-state";
import { fetchResourceChecks, startResourceChecks, type ResourceCheck, type ResourceKind, type ResourceTarget } from "./resource-check-api";
import { resourceCheckEn, resourceCheckZh } from "./resource-check-copy";
import { resourceFinding, resourceHistory } from "./resource-check-presentation";
import { ResourceCheckReport, ResourceFindingBadge } from "./resource-check-report";

export function ResourceCheckDialog({ kind, targets, onClose }: { kind: ResourceKind; targets: ResourceTarget[]; onClose: () => void }) {
  const { t, i18n } = useTranslation();
  const copy = i18n.language.startsWith("zh") ? resourceCheckZh : resourceCheckEn;
  const client = useQueryClient();
  const [selectedModel, setSelectedModel] = useState("");
  const [failures, setFailures] = useState<string[]>([]);
  const [selectedID, setSelectedID] = useState(targets[0]?.id ?? "");
  const [search, setSearch] = useState("");
  const content = useRef<HTMLDivElement>(null);
  const [submission, setSubmission] = useState(0);
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => { const timer = window.setInterval(() => setNow(Date.now()), 1000); return () => window.clearInterval(timer); }, []);
  const ids = targets.map(target => target.id).sort();
  const queryKey = ["quality", "resource-checks", kind, ids.join(",")];
  const checks = useQuery({ queryKey, queryFn: ({ signal }) => fetchResourceChecks(kind, ids, signal), refetchOnMount: "always", refetchInterval: 2000 });
  const models = useQuery({ queryKey: ["models", "quality-check-options"], queryFn: ({ signal }) => listModels({ page: 1, pageSize: 500, provider: "grok_build", status: "enabled" }, signal) });
  const options = [...new Set((models.data?.items ?? []).filter(route => route.capability === "responses" || route.capability === "chat").map(route => route.publicId))];
  const model = options.includes(selectedModel) ? selectedModel : options[0] || "";
  const activeIDs = new Set(checks.data?.filter(check => check.state === "pending" || check.state === "running").map(check => check.resource_id));
  const idleIDs = ids.filter(id => !activeIDs.has(id));
  const start = useLifetimeMutation({
    mutationFn: (_: void, signal) => startResourceChecks(kind, idleIDs, model, signal),
    onSuccess: async items => {
      setFailures(items.filter(item => item.error).map(item => `${targets.find(target => target.id === item.resource_id)?.name || item.resource_id}: ${t(`resourceChecks.submission.${item.error}`, { defaultValue: t("resourceChecks.submission.submission_failed") })}`));
      if (items.some(item => item.id)) setSubmission(value => value + 1);
      await client.invalidateQueries({ queryKey });
    },
    onError: error => showErrorToast(error, t),
  });
  const selected = targets.find(target => target.id === selectedID) ?? targets[0];
  const rows = targets.map(target => ({ target, history: resourceHistory(checks.data ?? [], kind, target.id) }));
  const totals = { healthy: 0, degraded: 0, inconclusive: 0, running: 0, empty: 0 };
  for (const row of rows) {
    const finding = resourceFinding(row.history[0]);
    totals[finding === "pending" ? "running" : finding === "cancelled" || finding === "failed" ? "inconclusive" : finding]++;
  }
  const filtered = rows.filter(({ target }) => `${target.name} ${target.id}`.toLocaleLowerCase().includes(search.trim().toLocaleLowerCase()));
  const batch = targets.length > 1;
  const TargetIcon = kind === "account" ? UserRound : Network;
  return <Dialog open onOpenChange={open => { if (!open) onClose(); }}>
    <DialogContent className={cn("flex max-h-[92dvh] flex-col gap-0 overflow-hidden p-0", batch ? "sm:max-w-6xl" : "sm:max-w-4xl")}>
      <DialogHeader className="shrink-0 border-b px-5 py-5 text-left sm:px-6"><DialogTitle className="pr-7 text-lg font-semibold">{t(`resourceChecks.title.${kind}`)}{batch && <span className="ml-2 text-sm font-normal text-muted-foreground">· {targets.length}</span>}</DialogTitle><DialogDescription>{t(`resourceChecks.description.${kind}`)}</DialogDescription></DialogHeader>
      <div ref={content} className="min-h-0 overflow-y-auto overscroll-contain p-5 sm:p-6">
        {models.isError && <ErrorState message={String(models.error)} onRetry={() => void models.refetch()} />}
        {failures.length > 0 && <div role="alert" className="mb-4 rounded-lg border border-destructive/25 bg-destructive/5 p-3 text-sm text-destructive"><p className="mb-1 font-medium">{copy.partial}</p>{failures.map(failure => <p key={failure} className="break-words">{failure}</p>)}</div>}
        {checks.isError ? <ErrorState message={String(checks.error)} onRetry={() => void checks.refetch()} /> : checks.isPending ? <div className="flex justify-center p-10"><Spinner /></div> : <>
          {batch && <section aria-label={copy.overview} className="mb-5"><dl className="grid grid-cols-3 gap-2 sm:grid-cols-5">{Object.entries(totals).map(([key, count]) => <div key={key} className="rounded-xl border bg-muted/20 px-3 py-2.5"><dt className="text-xs text-muted-foreground">{copy.totals[key as keyof typeof totals]}</dt><dd className={cn("mt-1 text-xl font-semibold tabular-nums", key === "degraded" && count > 0 && "text-rose-600 dark:text-rose-400", key === "healthy" && count > 0 && "text-emerald-600 dark:text-emerald-400")}>{count}</dd></div>)}</dl><p className="mt-2 text-xs text-muted-foreground">{copy.resourceHelp}</p></section>}
          <div className={cn("grid items-start gap-5", batch && "md:grid-cols-[240px_minmax(0,1fr)]")}>
            {batch && <nav className="min-w-0 space-y-3 md:sticky md:top-0" aria-label={copy.resources}>
              <div className="relative"><Search className="pointer-events-none absolute left-3 top-2.5 size-4 text-muted-foreground" /><input type="search" aria-label={copy.search} placeholder={copy.search} value={search} onChange={event => setSearch(event.target.value)} className="w-full rounded-lg border bg-background py-2 pl-9 pr-3 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring" /></div>
              <div className="max-h-48 space-y-1.5 overflow-y-auto overscroll-contain pr-1 md:max-h-[50dvh]">{filtered.map(({ target, history }) => <button key={target.id} type="button" aria-pressed={selected?.id === target.id} onClick={() => { setSelectedID(target.id); content.current?.scrollTo({ top: 0 }); }} className={cn("w-full rounded-xl border p-3 text-left transition-colors hover:bg-muted/50 focus-visible:outline-2 focus-visible:outline-ring", selected?.id === target.id ? "border-primary/40 bg-primary/5" : "border-transparent bg-muted/20")}>
                <div className="flex items-center gap-2"><TargetIcon className="size-4 shrink-0 text-muted-foreground" /><span className="min-w-0 flex-1 truncate text-sm font-medium" title={target.name}>{target.name}</span><ChevronRight className="size-3.5 shrink-0 text-muted-foreground" /></div><div className="mt-2 flex flex-wrap items-center justify-between gap-1.5"><span className="break-all text-[11px] text-muted-foreground">#{target.id}</span><ResourceFindingBadge check={history[0]} now={now} /></div>
              </button>)}{!filtered.length && <p className="p-3 text-sm text-muted-foreground">{copy.noMatch}</p>}</div>
            </nav>}
            {selected && <ResourceHistory key={`${selected.id}:${submission}`} target={selected} history={rows.find(row => row.target.id === selected.id)?.history ?? []} now={now} />}
          </div>
        </>}
      </div>
      <DialogFooter className="shrink-0 gap-3 border-t bg-muted/15 p-4 sm:items-end sm:justify-between sm:space-x-0 sm:px-6">
        <div className="min-w-0 space-y-1.5 sm:max-w-[60%]"><div className="flex items-center gap-2"><Label htmlFor="resource-check-model" className="shrink-0 text-xs">{t("resourceChecks.model")}</Label><select id="resource-check-model" className="min-w-0 max-w-full rounded-md border bg-background px-2 py-1.5 text-sm" value={model} onChange={event => setSelectedModel(event.target.value)} disabled={start.isPending || models.isPending || models.isError}>
          {!options.length && <option value="">{t("resourceChecks.noModels")}</option>}{options.map(name => <option key={name} value={name}>{name}</option>)}
        </select></div><p className="text-[11px] leading-5 text-muted-foreground">{copy.submitNote}</p>{ids.length > 32 && <p role="alert" className="text-xs text-destructive">{copy.limit}</p>}</div>
        <div className="flex shrink-0 justify-end gap-2"><Button variant="outline" onClick={onClose}>{t("common.close")}</Button><Button disabled={!model || models.isError || ids.length > 32 || idleIDs.length === 0 || checks.isPending || checks.isError || start.isPending} onClick={() => start.mutate()}>{start.isPending && <Spinner />}{idleIDs.length === 0 ? copy.allRunning : t("resourceChecks.start", { count: idleIDs.length })}</Button></div>
      </DialogFooter>
    </DialogContent>
  </Dialog>;
}

function ResourceHistory({ target, history, now }: { target: ResourceTarget; history: ResourceCheck[]; now: number }) {
  const { i18n } = useTranslation();
  const copy = i18n.language.startsWith("zh") ? resourceCheckZh : resourceCheckEn;
  const [selectedID, setSelectedID] = useState("");
  const check = history.find(item => item.id === selectedID) ?? history[0];
  const older = check && check !== history[0];
  return <section className="min-w-0 space-y-4" aria-label={target.name}>
    <div className="flex flex-wrap items-start justify-between gap-3"><div className="min-w-0"><p className="mb-1 text-xs text-muted-foreground">{copy.target} · #{target.id}</p><h2 className="break-words text-base font-semibold">{target.name}</h2></div>
      {history.length > 0 && <div className="min-w-0 max-w-full space-y-1"><Label htmlFor="resource-check-history" className="text-xs text-muted-foreground">{copy.history}</Label><select id="resource-check-history" className="block w-full max-w-full rounded-md border bg-background p-2 text-xs" value={check?.id ?? ""} onChange={event => setSelectedID(event.target.value === history[0]?.id ? "" : event.target.value)}>{history.map((item, index) => <option key={item.id} value={item.id}>{index === 0 ? `${copy.latest} · ` : ""}{new Date(item.created_at).toLocaleString()} · #{item.id}</option>)}</select></div>}
    </div>
    {older && <div className="flex flex-wrap items-center justify-between gap-2 rounded-lg bg-muted/40 px-3 py-2 text-xs text-muted-foreground"><span>{copy.older}</span><button className="font-medium text-primary underline underline-offset-4" onClick={() => setSelectedID("")}>{copy.backToLatest}</button></div>}
    {check ? <ResourceCheckReport key={check.id} check={check} target={target} now={now} /> : <div className="rounded-xl border border-dashed p-8 text-center"><CircleHelp className="mx-auto mb-3 size-7 text-muted-foreground" /><h3 className="font-medium">{copy.emptyTitle}</h3><p className="mx-auto mt-2 max-w-md text-sm leading-6 text-muted-foreground">{copy.emptyNote}</p></div>}
    <p className="border-t pt-3 text-xs leading-5 text-muted-foreground">{copy.actionNote}</p>
  </section>;
}
