import { useTranslation } from "react-i18next";
import type { ExecutionDiagnosticsDTO } from "@/entities/audit/audit-api";

const milliseconds = (us: number) => `${(us / 1000).toFixed(2)} ms`;

export function ExecutionDiagnosticsPanel({ value }: { value?: ExecutionDiagnosticsDTO }) {
  const { t } = useTranslation();
  if (!value) return <p className="text-sm text-muted-foreground">{t("audits.executionMissing")}</p>;
  const label = (stage: string) => t(`audits.executionStageNames.${stage}`, { defaultValue: stage });
  return <div className="space-y-5">
    <p className="text-xs leading-5 text-muted-foreground">{t("audits.executionHint")}</p>
    {value.truncated ? <p className="text-xs text-amber-700 dark:text-amber-300">{t("audits.executionTruncated")}</p> : null}
    {(value.failures ?? []).map((failure, index) => <div key={index} className="rounded border border-red-500/20 p-3 text-xs">
      <span className="font-medium">{failure.component} · {label(failure.stage)}</span><code className="ml-3 break-all">{failure.reason}</code>
    </div>)}
    <table className="w-full text-left text-xs"><thead><tr className="border-b"><th className="py-2">{t("audits.executionStage")}</th><th>{t("audits.executionDuration")}</th><th>{t("audits.executionCalls")}</th></tr></thead>
      <tbody>{(value.stages ?? []).map(stage => <tr key={stage.stage} className="border-b border-border/50"><td className="py-2">{label(stage.stage)}</td><td className="tabular-nums">{milliseconds(stage.us)}</td><td>{stage.calls}</td></tr>)}</tbody>
    </table>
    {(value.exchanges ?? []).map(exchange => <section key={exchange.physicalId} className="space-y-3 rounded-lg border p-3">
      <div className="flex flex-wrap justify-between gap-2 text-xs"><code className="break-all">{exchange.physicalId}</code><span>{exchange.plane} / {exchange.stage} · {exchange.status > 0 ? `HTTP ${exchange.status}` : t("audits.executionNoHeaders")} · {milliseconds(exchange.us)}</span></div>
      <p className="text-xs text-muted-foreground">{t("audits.executionPath", { account: exchange.accountId, node: exchange.nodeId, epoch: exchange.epoch })}</p>
      <ol className="grid gap-x-6 gap-y-1 text-xs sm:grid-cols-2">{(exchange.events ?? []).map((event,index) => <li key={index} className="flex justify-between gap-3 py-1"><span>{label(event.stage)}{event.stage === "got_conn" ? ` · ${t(event.reused ? "audits.executionReused" : "audits.executionNewConnection")}` : ""}{event.resumed ? ` · ${t("audits.executionTLSResumed")}` : ""}{event.failed ? ` · ${t("audits.executionFailed")}` : ""}</span><span className="shrink-0 tabular-nums">+{milliseconds(event.us)}</span></li>)}</ol>
      {exchange.prompt ? <details className="text-xs"><summary className="cursor-pointer py-1">{t("audits.executionCacheDigests")}</summary>
        <p className="my-2 leading-5 text-muted-foreground">{t("audits.executionDigestHint")}</p>
        <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-1">
          {(["keyEpoch", "session", "instructions", "tools", "parameters", "inputItems", "inputBytes"] as const).map(key => <div key={key} className="contents"><dt>{t(`audits.executionPrompt.${key}`)}</dt><dd className="break-all font-mono">{exchange.prompt?.[key]}</dd></div>)}
          {(exchange.prompt.prefixes ?? []).map(prefix => <div key={prefix.items} className="contents"><dt>{t("audits.executionPrefix", { count: prefix.items })}</dt><dd className="break-all font-mono">{prefix.digest}</dd></div>)}
        </dl>
      </details> : null}
    </section>)}
  </div>;
}
