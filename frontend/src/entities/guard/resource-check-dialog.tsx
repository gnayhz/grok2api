import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { listModels } from "@/entities/model/model-api";
import { useLifetimeMutation } from "@/shared/hooks/use-lifetime-mutation";
import { showErrorToast } from "@/shared/lib/show-error";
import { Button } from "@/shared/ui/button";
import { Badge } from "@/shared/ui/badge";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/shared/ui/dialog";
import { Spinner } from "@/shared/ui/spinner";
import { Label } from "@/shared/ui/label";
import { ErrorState } from "@/shared/components/data-state";
import { fetchResourceChecks, startResourceChecks, type ResourceCheck, type ResourceKind, type ResourceSample, type ResourceTarget } from "./resource-check-api";

export function ResourceCheckDialog({ kind, targets, onClose }: { kind: ResourceKind; targets: ResourceTarget[]; onClose: () => void }) {
  const { t } = useTranslation();
  const client = useQueryClient();
  const [selectedModel, setSelectedModel] = useState("");
  const [failures, setFailures] = useState<string[]>([]);
  const ids = targets.map(target => target.id).sort();
  const queryKey = ["quality", "resource-checks", kind, ids.join(",")];
  const checks = useQuery({ queryKey, queryFn: ({ signal }) => fetchResourceChecks(kind, ids, signal), refetchOnMount: "always", refetchInterval: 2000 });
  const models = useQuery({ queryKey: ["models", "quality-check-options"], queryFn: ({ signal }) => listModels({ page: 1, pageSize: 500, provider: "grok_build", status: "enabled" }, signal) });
  const options = [...new Set((models.data?.items ?? []).filter(route => route.capability === "responses" || route.capability === "chat").map(route => route.publicId))];
  const model = selectedModel || options[0] || "";
  const activeIDs = new Set(checks.data?.filter(check => check.state === "pending" || check.state === "running").map(check => String(check.resource_id)));
  const idleIDs = ids.filter(id => !activeIDs.has(id));
  const start = useLifetimeMutation({
    mutationFn: (_: void, signal) => startResourceChecks(kind, idleIDs, model, signal),
    onSuccess: items => { setFailures(items.filter(item => item.error).map(item => `${targets.find(target => target.id === String(item.resource_id))?.name || item.resource_id}: ${t(`resourceChecks.submission.${item.error}`, { defaultValue: t("resourceChecks.submission.submission_failed") })}`)); void client.invalidateQueries({ queryKey }); },
    onError: error => showErrorToast(error, t),
  });
  return <Dialog open onOpenChange={open => { if (!open) onClose(); }}>
    <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-3xl">
      <DialogHeader><DialogTitle>{t(`resourceChecks.title.${kind}`)} · {targets.length}</DialogTitle><DialogDescription>{t(`resourceChecks.description.${kind}`)}</DialogDescription></DialogHeader>
      <p className="text-sm">{t("resourceChecks.scope")}</p>
      <div className="space-y-2"><Label htmlFor="resource-check-model">{t("resourceChecks.model")}</Label><select id="resource-check-model" className="w-full rounded-md border bg-background p-2 text-sm" value={model} onChange={event => setSelectedModel(event.target.value)} disabled={start.isPending}>
        {!options.length && <option value="">{t("resourceChecks.noModels")}</option>}{options.map(name => <option key={name} value={name}>{name}</option>)}
      </select><p className="text-xs text-muted-foreground">{t("resourceChecks.budget")}</p></div>
      {models.isError && <ErrorState message={String(models.error)} onRetry={() => void models.refetch()} />}
      {failures.length > 0 && <div role="alert" className="text-sm text-destructive">{failures.map(failure => <p key={failure}>{failure}</p>)}</div>}
      {checks.isError ? <ErrorState message={String(checks.error)} onRetry={() => void checks.refetch()} /> : checks.isPending ? <Spinner /> : <div className="space-y-4" aria-live="polite">
        {targets.map(target => <section key={target.id} className="space-y-2"><p className="font-medium">{target.name}</p>
          {!checks.data?.some(check => String(check.resource_id) === target.id) && <p className="text-sm text-muted-foreground">{t("resourceChecks.empty")}</p>}
          {checks.data?.filter(check => String(check.resource_id) === target.id).map((check, index) => <ResourceResult key={check.id} check={check} latest={index === 0} />)}
        </section>)}
      </div>}
      <DialogFooter><Button variant="outline" onClick={onClose}>{t("common.close")}</Button><Button disabled={!model || ids.length > 32 || idleIDs.length === 0 || checks.isPending || checks.isError || start.isPending} onClick={() => start.mutate()}>{start.isPending && <Spinner />}{t("resourceChecks.start", { count: idleIDs.length })}</Button></DialogFooter>
    </DialogContent>
  </Dialog>;
}

function ResourceResult({ check, latest }: { check: ResourceCheck; latest: boolean }) {
  const { t } = useTranslation();
  const running = check.state === "pending" || check.state === "running";
  const outcome = running ? check.state : check.state === "done" ? check.report?.outcome ?? "inconclusive" : "inconclusive";
  return <div className="rounded-lg border p-3 space-y-2">
    <div className="flex flex-wrap items-center justify-between gap-2"><span className="text-sm">{check.model} · #{check.id} · {new Date(check.created_at).toLocaleString()}</span><Badge variant={outcome === "degraded" ? "destructive" : "secondary"}>{t(`resourceChecks.outcome.${outcome}`)}</Badge></div>
    <p className="text-sm">{running ? t("resourceChecks.accepted") : t(`resourceChecks.reason.${check.state === "done" ? check.report?.reason : "interrupted"}`, { defaultValue: t("resourceChecks.reason.measurement_unavailable") })}</p>
    {check.report && <><p className="text-xs text-muted-foreground">{t("resourceChecks.progress", { calls: check.report.calls, max: check.report.max_calls })}</p><details open={latest}><summary className="cursor-pointer text-sm">{t("resourceChecks.evidence")}</summary><div className="mt-2 space-y-2">
      {check.report.groups.map((group, index) => <div key={index} className="rounded border p-2 text-xs space-y-2">
        <p>{t("resourceChecks.group", { index: index + 1, account: group.account_id, node: group.node_id })} · {t(`resourceChecks.outcome.${group.outcome}`)}</p>
        <p>{t(`resourceChecks.reason.${group.reason}`, { defaultValue: group.reason })}</p>
        <p>{t("resourceChecks.control", { account: group.control_account, node: group.control_node })}</p>
        <p>{t("resourceChecks.vector", { target: group.samples.map(s => s.usage_reported ? s.input_tokens : "—").join(" / ") || "—", control: group.control.map(s => s.usage_reported ? s.input_tokens : "—").join(" / ") || "—", delta: group.delta, controlDelta: group.control_delta })}</p>
        <div className="overflow-x-auto"><table className="w-full text-left"><thead><tr><th>{t("resourceChecks.phase")}</th><th>{t("resourceChecks.thinking")}</th><th>{t("resourceChecks.complete")}</th><th>{t("resourceChecks.path")}</th></tr></thead><tbody>
          {group.control.map((sample, i) => <SampleRow key={`c${i}`} sample={sample} phase={t("resourceChecks.controlPhase")} />)}
          {group.samples.map((sample, i) => <SampleRow key={`s${i}`} sample={sample} phase={t("resourceChecks.targetPhase")} />)}
          {group.after && <SampleRow sample={group.after} phase={t("resourceChecks.afterPhase")} />}
        </tbody></table></div>
      </div>)}
    </div></details></>}
  </div>;
}
function SampleRow({ sample, phase }: { sample: ResourceSample; phase: string }) {
  const { t } = useTranslation();
  return <tr><td>{phase} · {sample.sample === "token-long" ? "2048" : "512"}</td><td>{t(sample.thinking ? "resourceChecks.yes" : "resourceChecks.no")}</td><td>{t(sample.completed ? "resourceChecks.yes" : "resourceChecks.no")}</td><td>{sample.path_verified ? `IPv${sample.path_family} · ${sample.path_key?.slice(0, 10)}` : t("resourceChecks.unverified")}{sample.failure && ` · ${sample.failure}`}</td></tr>;
}
