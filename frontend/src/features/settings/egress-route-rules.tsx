import { useQuery } from "@tanstack/react-query";
import { CircleHelp } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  getEgressRouteRuleStats,
  type EgressNodeDTO,
  type EgressRouteRuleDTO,
  type EgressRouteRuleStatDTO,
  type EgressRouteRuleTargetMode,
  type EgressTrafficClass,
} from "@/features/settings/settings-api";
import { formatDateTime } from "@/shared/lib/format";

const routeRuleClasses: EgressTrafficClass[] = ["inference", "credential", "billing", "model_sync", "video"];
const routeRuleClassLabelKeys: Record<EgressTrafficClass, string> = {
  inference: "settings.egress.routeClassInference",
  credential: "settings.egress.routeClassCredential",
  billing: "settings.egress.routeClassBilling",
  model_sync: "settings.egress.routeClassModelSync",
  video: "settings.egress.routeClassVideo",
};
const routeRuleClassHelpKeys: Record<EgressTrafficClass, string> = {
  inference: "settings.egress.routeClassInferenceHelp",
  credential: "settings.egress.routeClassCredentialHelp",
  billing: "settings.egress.routeClassBillingHelp",
  model_sync: "settings.egress.routeClassModelSyncHelp",
  video: "settings.egress.routeClassVideoHelp",
};

type RouteRulesPanelProps = {
  rules: EgressRouteRuleDTO[];
  candidates: EgressNodeDTO[];
  onChange: (next: EgressRouteRuleDTO[]) => void;
};


/** Inline per-class outcome counters. Process-local: they reset on restart. */
function RouteRuleStatBadges({ stat }: { stat?: EgressRouteRuleStatDTO }) {
  const { t, i18n } = useTranslation();
  if (!stat) return null;
  const fallback = stat.nodeUnavailable + stat.directUnavailable;
  return (
    <div className="mt-1 flex flex-wrap items-center gap-1.5 text-[11px] leading-4 text-muted-foreground">
      <span className="text-emerald-600 dark:text-emerald-400">{t("settings.egress.routeStatHit", { count: stat.hit })}</span>
      {stat.skippedBinding > 0 ? <span>· {t("settings.egress.routeStatSkipped", { count: stat.skippedBinding })}</span> : null}
      {fallback > 0 ? (
        <span className="font-medium text-amber-600 dark:text-amber-400">· {t("settings.egress.routeStatFallback", { count: fallback })}</span>
      ) : null}
      {stat.lastSeen ? <span>· {t("settings.egress.routeStatLastSeen", { time: formatDateTime(stat.lastSeen, i18n.language) })}</span> : null}
    </div>
  );
}

/**
 * Traffic-class egress routing for Grok Build. Each row pins one call purpose
 * (inference, OAuth, billing, model sync, video) to a dedicated egress node
 * or a direct connection. Unconfigured classes keep the existing scope-pool
 * behavior, and account-bound inference/video always honor their binding.
 */
export function EgressRouteRulesPanel({ rules, candidates, onChange }: RouteRulesPanelProps) {
  const { t } = useTranslation();
  // Stats are display-only; the query stays silent so a failed fetch never
  // blocks rule editing.
  const statsQuery = useQuery({
    queryKey: ["egress-route-rule-stats"],
    queryFn: getEgressRouteRuleStats,
    refetchInterval: 30_000,
  });

  const statFor = (trafficClass: EgressTrafficClass) =>
    statsQuery.data?.items.find((item) => item.scope === "grok_build" && item.class === trafficClass);

  const ruleFor = (trafficClass: EgressTrafficClass) => rules.find((rule) => rule.scope === "grok_build" && rule.class === trafficClass);

  function setRule(trafficClass: EgressTrafficClass, next: EgressRouteRuleDTO | null) {
    const remaining = rules.filter((rule) => !(rule.scope === "grok_build" && rule.class === trafficClass));
    onChange(next ? [...remaining, next] : remaining);
  }

  function setMode(trafficClass: EgressTrafficClass, mode: EgressRouteRuleTargetMode) {
    const current = ruleFor(trafficClass);
    let nodeId: string | undefined;
    if (mode === "fixed") {
      // Never persist a fixed rule without a concrete node: the saved rule
      // would render as an empty Select after reload (no matching item), which
      // is exactly the "last row loses its selection after refresh" symptom.
      if (candidates.length === 0) return;
      nodeId = candidates.find((node) => node.id === current?.targetNodeId)?.id ?? candidates[0]?.id;
    }
    setRule(trafficClass, { scope: "grok_build", class: trafficClass, targetMode: mode, targetNodeId: nodeId, enabled: current?.enabled ?? true });
  }

  return (
    <div className="pt-4">
      <div className="flex items-center gap-1.5 px-0.5">
        <h3 className="text-sm font-medium tracking-tight">{t("settings.egress.routeRules")}</h3>
        <Tooltip>
          <TooltipTrigger asChild>
            <button type="button" className="text-muted-foreground transition-colors hover:text-foreground" aria-label={t("settings.egress.routeRulesHelp")}>
              <CircleHelp className="size-3.5" />
            </button>
          </TooltipTrigger>
          <TooltipContent className="max-w-100">{t("settings.egress.routeRulesHelp")}</TooltipContent>
        </Tooltip>
      </div>
      <p className="mt-1 px-0.5 text-xs leading-5 text-muted-foreground">{t("settings.egress.routeRulesScopeHint")}</p>
      <div className="mt-3 space-y-2">
        {routeRuleClasses.map((trafficClass) => {
          const rule = ruleFor(trafficClass);
          const mode = rule?.targetMode ?? "none";
          const selectedAvailable = !rule?.targetNodeId || candidates.some((node) => node.id === rule.targetNodeId);
          // Resolve the effective value first so the "unavailable" option is
          // rendered whenever it is selected — a Radix Select whose value has
          // no matching item displays an empty trigger (the refresh bug), so
          // every possible value must have a rendered SelectItem.
          const nodeValue = selectedAvailable ? (rule?.targetNodeId ?? "unavailable") : "unavailable";
          return (
            <div className="grid min-w-0 gap-2.5 py-1 sm:grid-cols-[minmax(0,2fr)_minmax(0,1fr)] sm:items-center sm:gap-8" key={trafficClass}>
              <div className="min-w-0">
                <div className="flex min-h-5 items-center gap-2">
                  <Label className="text-xs font-medium">{t(routeRuleClassLabelKeys[trafficClass])}</Label>
                  {rule?.enabled === false ? <Badge variant="secondary">{t("settings.egress.routeRuleDisabled")}</Badge> : null}
                </div>
                <p className="mt-1 max-w-xl text-xs leading-5 text-muted-foreground">{t(routeRuleClassHelpKeys[trafficClass])}</p>
                <RouteRuleStatBadges stat={statFor(trafficClass)} />
              </div>
              <div className={mode === "fixed" ? "grid min-w-0 gap-2 sm:grid-cols-2" : "grid min-w-0"}>
                <Select
                  value={mode}
                  onValueChange={(next) => (next === "none" ? setRule(trafficClass, null) : setMode(trafficClass, next as EgressRouteRuleTargetMode))}
                >
                  <SelectTrigger aria-label={t("settings.egress.routeRuleMode", { trafficClass: t(routeRuleClassLabelKeys[trafficClass]) })}>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="none">{t("settings.egress.routeRuleNone")}</SelectItem>
                    <SelectItem value="direct">{t("settings.egress.routeRuleDirect")}</SelectItem>
                    <SelectItem value="fixed" disabled={candidates.length === 0}>{t("settings.egress.routeRuleFixed")}</SelectItem>
                  </SelectContent>
                </Select>
                {mode === "fixed" ? (
                  <Select
                    /* While node candidates are still loading, a controlled value
                       has no matching SelectItem and the Radix trigger renders
                       blank — indistinguishable from a lost selection (seen on
                       slow links as "last row empty after refresh"). Withhold the
                       value until candidates arrive so the placeholder shows. */
                    value={candidates.length === 0 ? undefined : nodeValue}
                    disabled={candidates.length === 0}
                    onValueChange={(nodeId) => setRule(trafficClass, { scope: "grok_build", class: trafficClass, targetMode: "fixed", targetNodeId: nodeId, enabled: rule?.enabled ?? true })}
                  >
                    <SelectTrigger aria-label={t("settings.egress.routeRuleNode", { trafficClass: t(routeRuleClassLabelKeys[trafficClass]) })}>
                      <SelectValue placeholder={t("common.loading")} />
                    </SelectTrigger>
                    <SelectContent>
                      {nodeValue === "unavailable" ? <SelectItem value="unavailable" disabled>{t("settings.egress.fallbackNodeUnavailable")}</SelectItem> : null}
                      {candidates.map((node) => (
                        <SelectItem key={node.id} value={node.id}>{node.name}</SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                ) : null}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}
