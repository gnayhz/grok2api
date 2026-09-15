import { Activity, Check, ChevronDown, Layers3 } from "lucide-react";
import { useTranslation } from "react-i18next";

import { CopyButton } from "@/shared/components/copy-button";
import { cn } from "@/shared/lib/cn";
import { formatNumber } from "@/shared/lib/format";
import { formatUSDTicks } from "@/shared/lib/usd";
import type { AuditGenerationUsageDTO } from "./request-audits-api";
import { auditCachedUsageAvailable } from "./audit-usage";

export function GenerationUsagePanel({ values }: { values: AuditGenerationUsageDTO[] }) {
  const { t, i18n } = useTranslation();
  const number = (value: AuditGenerationUsageDTO, count: number) => value.usageSource === "none" ? "—" : formatNumber(count, i18n.language);

  return (
    <div className="space-y-4">
      <div className="flex items-start gap-2.5 rounded-lg border bg-muted/20 p-4">
        <Layers3 className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
        <p className="text-xs leading-6 text-muted-foreground">{t("audits.generationUsageNote")}</p>
      </div>
      {values.length === 0 ? <div className="flex flex-col items-center gap-3 py-16 text-center text-muted-foreground"><Activity className="size-8 stroke-1" /><p className="max-w-md text-sm leading-6">{t("audits.noGenerationUsage")}</p></div> : values.map((value) => (
        <article key={value.physicalId} className={cn("overflow-hidden rounded-xl border", value.selected && "border-foreground/25")}>
          <header className="flex flex-wrap items-start justify-between gap-3 border-b bg-muted/15 p-4">
            <div className="flex min-w-0 items-start gap-3">
              <span className="flex size-8 shrink-0 items-center justify-center rounded-lg border bg-background text-xs font-medium tabular-nums">{value.ordinal}</span>
              <div className="min-w-0">
                <h3 className="break-words text-sm font-medium">{value.accountName || "#" + value.accountId}</h3>
                <p className="mt-1 break-all text-xs text-muted-foreground">{value.model}</p>
              </div>
            </div>
            <div className="flex flex-wrap items-center gap-2 text-xs">
              {value.selected ? <span className="inline-flex items-center gap-1 rounded-md bg-foreground px-2 py-1 text-background"><Check className="size-3" />{t("audits.generationSelected")}</span> : <span className="rounded-md bg-muted px-2 py-1 text-muted-foreground">{t("audits.generationUnselected")}</span>}
              <span className={cn(value.outcome === "failed" ? "text-red-700 dark:text-red-300" : value.outcome === "completed" ? "text-emerald-700 dark:text-emerald-300" : "text-amber-700 dark:text-amber-300")}>{t("audits.completionOutcomes." + value.outcome)}</span>
            </div>
          </header>
          <div className="grid grid-cols-2 gap-x-6 gap-y-4 p-4 sm:grid-cols-4">
            <UsageValue label={t("audits.input")} value={number(value, value.inputTokens)} />
            <UsageValue label={t("audits.output")} value={number(value, value.outputTokens)} />
            <UsageValue label={t("audits.totalTokens")} value={number(value, value.totalTokens)} />
            <UsageValue label={t("audits.generationReportedCost")} value={value.costInUsdTicks > 0 ? formatUSDTicks(value.costInUsdTicks, 6) : "—"} />
          </div>
          <div className="flex flex-wrap items-center gap-x-5 gap-y-2 border-t bg-muted/10 px-4 py-3 text-xs text-muted-foreground">
            <span title={auditCachedUsageAvailable(value) ? undefined : t(value.cachedInputTokensReported === false ? "audits.cacheNotReported" : "audits.cacheUnknown")}>{t("audits.cached")} <span className="tabular-nums text-foreground">{auditCachedUsageAvailable(value) ? number(value, value.cachedInputTokens) : "—"}</span></span>
            <span>{t("audits.reasoning")} <span className="tabular-nums text-foreground">{number(value, value.reasoningTokens)}</span></span>
            <span className="sm:ml-auto">{t("audits.generationSources." + value.usageSource)}</span>
          </div>
          <details className="group/usage border-t">
            <summary className="flex cursor-pointer items-center gap-2 px-4 py-3 text-xs text-muted-foreground outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-foreground/30"><ChevronDown className="size-3.5 -rotate-90 transition-transform group-open/usage:rotate-0" />{t("audits.moreUsageDetails")}</summary>
            <div className="space-y-4 border-t p-4">
              <div className="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-3">
                <UsageValue label={t("audits.generationCacheCreation")} value={number(value, value.cacheCreationTokens)} />
                <UsageValue label={t("audits.contextInput")} value={number(value, value.contextInputTokens)} />
                <UsageValue label={t("audits.contextOutput")} value={number(value, value.contextOutputTokens)} />
                <UsageValue label={t("audits.generationSourcesCount")} value={number(value, value.numSourcesUsed)} />
                <UsageValue label={t("audits.generationToolsCount")} value={number(value, value.numServerSideToolsUsed)} />
                <UsageValue label={t("audits.estimatedCost")} value={value.pricingModel ? formatUSDTicks(value.estimatedCostInUsdTicks, 6) : "—"} />
                {value.pricingModel ? <UsageValue label={t("audits.billingModel")} value={value.pricingModel} /> : null}
                {value.pricingVersion ? <UsageValue label={t("audits.billingVersion")} value={value.pricingVersion} /> : null}
              </div>
              <div className="flex items-center gap-2 border-t pt-3 text-xs text-muted-foreground"><span className="shrink-0">{t("audits.physicalCallId")}</span><code className="min-w-0 flex-1 break-all">{value.physicalId}</code><CopyButton value={value.physicalId} /></div>
            </div>
          </details>
        </article>
      ))}
    </div>
  );
}

function UsageValue({ label, value }: { label: string; value: string }) {
  return <div className="min-w-0"><p className="text-xs text-muted-foreground">{label}</p><p className="mt-1.5 break-words text-sm font-medium tabular-nums">{value}</p></div>;
}
