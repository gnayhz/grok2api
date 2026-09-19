import { useTranslation } from "react-i18next";
import type { QualityCase, QualityProbeTask } from "@/entities/guard/quality-api";
import { Badge } from "@/shared/ui/badge";
import { Button } from "@/shared/ui/button";
import { Spinner } from "@/shared/ui/spinner";
import { partyDispositionKey } from "./quality-case-presentation";

export function CaseProofEvidence({ item, tasks, loading, error, retry }: {
  item: QualityCase; tasks: QualityProbeTask[]; loading: boolean; error: boolean; retry: () => void;
}) {
  const { t, i18n } = useTranslation();
  const zh = i18n.language.startsWith("zh");
  return <section className="space-y-3 text-sm">
    <p>{zh ? "与主动风控检测使用相同的完整观测和 R1/R2/R3 规则。正常证据来自本轮 A 型响应；证明充分即停止，失败不作为 B，冲突不按多数票裁决。" : "Uses the same complete observations and R1/R2/R3 rules as active checks. A responses establish controls; stop once proved. Failures are not B, and conflicting evidence is not resolved by voting."}</p>
    <div className="grid gap-2 sm:grid-cols-2">{item.parties.map(p => <div className="rounded border p-2" key={`${p.kind}:${p.account_id}:${p.node_id}`}>
      {p.kind === "account" ? (zh ? "账号" : "Account") : (zh ? "出口" : "Exit")} #{p.kind === "account" ? p.account_id : p.node_id}
      <Badge className="ml-2" variant={p.disposition === "sentenced" ? "destructive" : "secondary"}>{t(partyDispositionKey(p.disposition))}</Badge>
    </div>)}</div>
    <p className="text-xs text-muted-foreground">{zh ? "本案释放仅表示本案不再限制该资源；无法判定不等于正常。历史证明保留测量时的结论，不代表资源永久正常或异常。" : "Release ends this case's restriction. Inconclusive does not mean normal. Historical proofs describe the measurement window, not permanent health."}</p>
    {loading && <Spinner />}
    {error && <div role="alert">{zh ? "证据读取失败" : "Unable to load evidence"} <Button variant="outline" onClick={retry}>{t("common.retry")}</Button></div>}
    {!loading && !error && tasks.length === 0 && <p>{zh ? "等待建立证明任务" : "Waiting for the proof task"}</p>}
    {tasks.filter(task => task.direction === "case_proof").map(original => { const task = { ...original, proof: item.proof ?? original.proof }; return ( <div key={task.id} className="space-y-3 rounded border p-3">
      <p>{zh ? "证明任务" : "Proof task"} #{task.id} · {t(`resourceChecks.outcome.${task.state}`, { defaultValue: task.state })}</p>
      {task.proof && <>
        <p>{t("resourceChecks.progress", { calls: task.proof.calls, max: task.proof.max_calls })} · {t("resourceChecks.consumption", { generations: task.proof.generations ?? 0, paths: task.proof.path_checks ?? 0 })}</p>
        <div className="space-y-2">{task.proof.results?.map(proof => <div key={`${proof.kind}:${proof.resource_id}`} className="rounded border p-2">
          <p>{proof.kind === "account" ? (zh ? "账号" : "Account") : (zh ? "出口" : "Exit")} #{proof.resource_id} · <Badge variant={proof.outcome === "degraded" ? "destructive" : "secondary"}>{t(`resourceChecks.outcome.${proof.outcome}`)}</Badge></p>
          <p>{t(`resourceChecks.reason.${proof.reason}`, { defaultValue: t(`experiment.facts.${proof.reason}`, { defaultValue: proof.reason }) })}</p>
          <p className="text-muted-foreground">{zh ? "证据有效期至" : "Evidence valid until"} {new Date(proof.valid_until).toLocaleString(i18n.language)}</p>
          <p>{t("resourceChecks.proof", { rule: proof.rule || "—", evidence: proof.evidence.map(id => `#${id}`).join(", ") || "—" })}</p>
        </div>)}</div>
        <div className="overflow-x-auto"><table className="w-full text-left text-xs"><thead><tr>
          <th>#</th><th>{zh ? "账号 × 出口" : "Account × exit"}</th><th>{zh ? "观测" : "Observation"}</th><th>{t("resourceChecks.thinking")}</th><th>{t("resourceChecks.complete")}</th><th>{t("resourceChecks.path")}</th>
        </tr></thead><tbody>{task.proof.observations?.map(o => <tr key={o.id}>
          <td>{o.id}</td><td>#{o.account_id} × #{o.node_id}</td><td>{t(`resourceChecks.observationClass.${o.class}`, {defaultValue:o.class})}</td><td>{t(o.sample.thinking ? "resourceChecks.yes" : "resourceChecks.no")}</td><td>{t(o.sample.completed ? "resourceChecks.yes" : "resourceChecks.no")}</td><td>{o.sample.path_verified ? `IPv${o.sample.path_family} · ${o.sample.path_key?.slice(0,10)}` : t("resourceChecks.unverified")}{o.sample.failure && ` · ${o.sample.failure}`}</td>
        </tr>)}</tbody></table></div>
      </>}
    </div>); })}
  </section>;
}
