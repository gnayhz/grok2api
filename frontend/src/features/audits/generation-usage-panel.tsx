import { useTranslation } from "react-i18next";
import type { AuditGenerationUsageDTO } from "./request-audits-api";
import { Badge } from "@/components/ui/badge";
import { formatNumber } from "@/shared/lib/format";
import { formatUSDTicks } from "@/shared/lib/usd";

export function GenerationUsagePanel({ values }: { values: AuditGenerationUsageDTO[] }) {
  const { t, i18n } = useTranslation();
  const number = (v: AuditGenerationUsageDTO, n: number) => v.usageSource === "none" ? "—" : formatNumber(n, i18n.language);

  return (
    <div className="space-y-4">
      <p className="text-xs leading-relaxed text-muted-foreground">{t("audits.generationUsageNote")}</p>
      {values.length === 0 ? <p className="py-8 text-center text-xs text-muted-foreground">{t("audits.noGenerationUsage")}</p> : (
        <div className="overflow-x-auto">
          <table className="w-full text-left text-xs">
            <thead className="border-b text-muted-foreground">
              <tr>
                {["generationAccount", "generationSelection", "generationCounts", "generationContext", "generationCost"].map((key) => (
                  <th key={key} className="whitespace-nowrap px-3 py-2 font-medium">{t(`audits.${key}`)}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {values.map((v) => (
                <tr key={v.physicalId} className="border-b align-top last:border-0">
                  <td className="space-y-1 px-3 py-3">
                    <p className="whitespace-nowrap font-medium">#{v.ordinal} · {v.accountName || v.accountId}</p>
                    <p className="text-muted-foreground">{v.model}</p>
                    <p className="text-[10px] text-muted-foreground" title={v.physicalId}>{v.physicalId}</p>
                  </td>
                  <td className="space-y-2 whitespace-nowrap px-3 py-3">
                    <Badge variant={v.selected ? "secondary" : "outline"}>{t(v.selected ? "audits.generationSelected" : "audits.generationUnselected")}</Badge>
                    <p>{t(`audits.completionOutcomes.${v.outcome}`)}</p>
                    <p className="text-muted-foreground">{t(`audits.generationSources.${v.usageSource}`)}</p>
                  </td>
                  <td className="space-y-1 whitespace-nowrap px-3 py-3 tabular-nums">
                    <p>{t("audits.input")} {number(v, v.inputTokens)} · {t("audits.output")} {number(v, v.outputTokens)}</p>
                    <p>{t("audits.cached")} {number(v, v.cachedInputTokens)} · {t("audits.reasoning")} {number(v, v.reasoningTokens)}</p>
                    <p>{t("audits.generationCacheCreation")} {number(v, v.cacheCreationTokens)}</p>
                    <p>{t("audits.total")} {number(v, v.totalTokens)}</p>
                  </td>
                  <td className="space-y-1 whitespace-nowrap px-3 py-3 tabular-nums">
                    <p>{t("audits.input")} {number(v, v.contextInputTokens)} · {t("audits.output")} {number(v, v.contextOutputTokens)}</p>
                    <p>{t("audits.generationSourcesCount")} {number(v, v.numSourcesUsed)}</p>
                    <p>{t("audits.generationToolsCount")} {number(v, v.numServerSideToolsUsed)}</p>
                  </td>
                  <td className="space-y-1 whitespace-nowrap px-3 py-3 tabular-nums">
                    <p>{t("audits.generationReportedCost")} {v.costInUsdTicks > 0 ? formatUSDTicks(v.costInUsdTicks, 6) : "—"}</p>
                    <p>{t("audits.estimated")} {v.pricingModel ? formatUSDTicks(v.estimatedCostInUsdTicks, 6) : "—"}</p>
                    {v.pricingModel ? <p className="text-[10px] text-muted-foreground">{v.pricingModel} · {v.pricingVersion}</p> : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
