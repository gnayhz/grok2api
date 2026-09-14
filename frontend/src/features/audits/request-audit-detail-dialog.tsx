import { useQuery } from "@tanstack/react-query";
import {
  Activity,
  Check,
  CheckCircle2,
  ChevronDown,
  ChevronRight,
  CircleDollarSign,
  CircleHelp,
  Clock3,
  FileText,
  ListChecks,
  Minus,
  X,
  Zap,
  KeyRound,
  ListTree,
  Network,
  Server,
  ShieldAlert,
  TriangleAlert,
} from "lucide-react";
import { useEffect, useMemo, useRef, useState, type ReactNode, type RefObject } from "react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { AuditResultBadge } from "./audit-result-badge";
import { auditBilling, auditResult, completionTone } from "./audit-presentation";
import { Badge } from "@/components/ui/badge";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { GenerationUsagePanel } from "./generation-usage-panel";
import { getRequestAudit, type AuditAttemptDTO, type AuditBillingBreakdownDTO, type AuditBillingComponentDTO, type AuditDTO } from "@/features/audits/request-audits-api";
import { CopyButton } from "@/shared/components/copy-button";
import { ErrorState, LoadingState } from "@/shared/components/data-state";
import { useAfterPaint } from "@/shared/hooks/use-after-paint";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatDuration, formatNumber } from "@/shared/lib/format";
import { formatUSDTicks, usdTicksToValue } from "@/shared/lib/usd";

const AUDIT_DETAIL_CACHE_TIME_MS = 60_000;
const PRE_UPSTREAM_ERROR_CODES = new Set([
  "model_not_allowed",
  "upstream_cooling",
  "upstream_model_cooling",
  "upstream_model_unavailable",
  "upstream_quota_exhausted",
  "upstream_saturated",
  "upstream_unavailable",
]);

export function RequestAuditDetailDialog({
  audit,
  open,
  onOpenChange,
  onReturnFocus,
}: {
  audit: AuditDTO | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onReturnFocus?: () => void;
}) {
  const titleRef = useRef<HTMLHeadingElement>(null);
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        overlayClassName="audit-detail-overlay"
        onOpenAutoFocus={(event) => { event.preventDefault(); titleRef.current?.focus({ preventScroll: true }); }}
        onCloseAutoFocus={(event) => { if (onReturnFocus) { event.preventDefault(); onReturnFocus(); } }}
        className="audit-detail flex h-[min(860px,calc(100svh-2rem))] max-h-[calc(100svh-2rem)] min-h-0 w-[calc(100%-1rem)] flex-col gap-0 overflow-hidden rounded-xl p-0 text-sm sm:w-[calc(100%-3rem)] sm:max-w-[1080px]"
      >
        <RequestAuditDetailContent audit={audit} open={open} titleRef={titleRef} />
      </DialogContent>
    </Dialog>
  );
}

// Radix keeps content mounted through the exit animation, then releases its
// query observer and tab state. Reopening starts from the overview again.
function RequestAuditDetailContent({ audit, open, titleRef }: {
  audit: AuditDTO | null;
  open: boolean;
  titleRef: RefObject<HTMLHeadingElement | null>;
}) {
  const { t, i18n } = useTranslation();
  const [tab, setTab] = useState("overview");
  const bodyReady = useAfterPaint();
  const detailQuery = useQuery({
    queryKey: ["request-audits", "detail", audit?.id],
    queryFn: ({ signal }) => getRequestAudit(audit?.id ?? "", signal),
    enabled: open && audit !== null,
    gcTime: AUDIT_DETAIL_CACHE_TIME_MS,
  });
  const activeAudit = detailQuery.data?.audit ?? audit;
  const attempts = detailQuery.data?.attempts ?? [];
  const hasDetail = Boolean(detailQuery.data);
  const failureCount = hasDetail ? attempts.length : (activeAudit?.attemptCount ?? 0);

  return (
    <>
        <DialogHeader className="shrink-0 border-b bg-muted/20 px-4 py-4 pr-12 text-left sm:px-6 sm:py-5 sm:pr-12">
          <div className="flex h-5 items-center justify-between gap-3">
            <DialogTitle ref={titleRef} tabIndex={-1} className="flex items-center gap-2 text-xs font-medium text-muted-foreground outline-none"><FileText className="size-3.5" />{t("audits.detailTitle")}</DialogTitle>
            <span role="status" className="flex shrink-0 items-center gap-1.5 text-[11px] text-muted-foreground">
              {detailQuery.isPending ? <><Spinner className="size-3.5" /><span className="sr-only sm:not-sr-only">{t("audits.loadingDiagnostics")}</span></> : null}
            </span>
          </div>
          {activeAudit ? <>
            <div className="flex min-w-0 flex-wrap items-center gap-2.5 pt-1">
              <h2 className="min-w-0 break-all text-xl font-medium tracking-tight">{activeAudit.modelPublicId || "#" + activeAudit.modelRouteId}</h2>
              <AuditResultBadge audit={activeAudit} />
              <span className="text-xs tabular-nums text-muted-foreground">{activeAudit.statusCode ? "HTTP " + activeAudit.statusCode : "HTTP —"}</span>
            </div>
            <DialogDescription asChild>
              <div className="flex min-w-0 flex-wrap items-center gap-x-4 gap-y-1 pt-1 text-xs text-muted-foreground">
                <div className="flex min-w-0 max-w-full items-center gap-1">
                  <span className="truncate font-mono" title={activeAudit.requestId}>{activeAudit.requestId}</span>
                  {activeAudit.requestId ? <CopyButton value={activeAudit.requestId} copyLabel={t("audits.copyRequestId")} className="size-6 [&_svg]:size-3" /> : null}
                </div>
                <time dateTime={activeAudit.createdAt}>{formatDateTime(activeAudit.createdAt, i18n.language)}</time>
                <span>{t("audits.operations." + activeAudit.operation)} · {t(activeAudit.streaming ? "audits.stream" : "audits.nonStream")}</span>
              </div>
            </DialogDescription>
          </> : <DialogDescription>{t("audits.pageDescription")}</DialogDescription>}
        </DialogHeader>

        {detailQuery.isPending && !activeAudit ? <LoadingState className="min-h-0 flex-1" /> : null}
        {activeAudit ? (
          <Tabs value={tab} onValueChange={setTab} className="flex min-h-0 flex-1 flex-col overflow-hidden">
            <div className="shrink-0 overflow-x-auto border-b px-4 sm:px-6">
              <TabsList showIndicator={false} aria-label={t("audits.detailSections")} className="h-12 w-max gap-4 rounded-none bg-transparent p-0 sm:gap-6">
                <TabsTrigger value="overview" className={detailTabClass}><FileText className="size-3.5" />{t("audits.requestOverview")}</TabsTrigger>
                <TabsTrigger value="attempts" className={detailTabClass} disabled={!hasDetail}><Network className="size-3.5" />{t("audits.failureDiagnostics")}
                  {failureCount > 0 ? (
                    <span className="min-w-5 rounded bg-red-500/10 px-1.5 py-0.5 text-[10px] font-medium tabular-nums text-red-700 dark:text-red-300">
                      {failureCount}
                    </span>
                  ) : null}
                </TabsTrigger>
                <TabsTrigger value="generation" className={detailTabClass} disabled={!hasDetail}><Activity className="size-3.5" />{t("audits.generationUsage")}</TabsTrigger>
                <TabsTrigger value="requestMetadata" className={detailTabClass} disabled={!hasDetail}><ListTree className="size-3.5" />{t("audits.requestMetadata")}</TabsTrigger>
              </TabsList>
            </div>
            {detailQuery.isError ? <div className="shrink-0 border-b"><ErrorState message={detailQuery.error.message} onRetry={() => void detailQuery.refetch()} /></div> : null}
            <TabsContent value="overview" className="min-h-0 flex-1 overflow-y-auto overscroll-contain p-4 sm:p-6">
              {bodyReady ? <RequestOverviewPanel audit={activeAudit} diagnosticsAvailable={hasDetail} onDiagnostics={() => setTab("attempts")} /> : <LoadingState className="h-full min-h-0" />}
            </TabsContent>
            <TabsContent value="requestMetadata" className="min-h-0 flex-1 overflow-hidden p-4 sm:p-6">
              {hasDetail ? <RequestMetadataPanel audit={activeAudit} /> : null}
            </TabsContent>
            <TabsContent value="generation" className="min-h-0 flex-1 overflow-y-auto overscroll-contain p-4 sm:p-6">
              {hasDetail ? <GenerationUsagePanel values={detailQuery.data?.generationUsages ?? []} /> : null}
            </TabsContent>
            <TabsContent value="attempts" className="min-h-0 flex-1 overflow-hidden">
              {hasDetail ? <UpstreamAttemptsPanel audit={activeAudit} attempts={attempts} /> : null}
            </TabsContent>
          </Tabs>
        ) : null}
    </>
  );
}

const detailTabClass = "audit-detail-tab h-12 gap-1.5 rounded-none border-b-2 border-transparent px-0 text-xs data-[state=active]:border-foreground disabled:opacity-40";

function RequestOverviewPanel({ audit, diagnosticsAvailable, onDiagnostics }: { audit: AuditDTO; diagnosticsAvailable: boolean; onDiagnostics: () => void }) {
  const { t, i18n } = useTranslation();
  const result = auditResult(audit);
  const billing = auditBilling(audit);
  const usageAvailable = audit.usageSource !== "none";
  const Icon = result === "failed" ? TriangleAlert : result === "success" ? CheckCircle2 : CircleHelp;
  const summary = audit.errorCode && audit.statusCode >= 200 && audit.statusCode < 300
    ? t("audits.streamFailedAfterHeaders")
    : result === "failed" ? t("audits.failedRequestHint")
    : audit.attemptCount > 0 ? t("audits.recoveredRequestHint", { count: audit.attemptCount })
    : result === "success" ? t("audits.successRequestHint") : t("audits.unknownRequestHint");
  const completionGroups = [
    { title: "generationAndDelivery", stages: ["admissionOutcome", "generationOutcome", "deliveryOutcome"] },
    { title: "savedState", stages: ["historyCommit", "providerStateCommit", "ownershipCommit"] },
    { title: "receiptsAndBilling", stages: ["physicalReceipt", "qualityReceipt", "ledgerOutcome"] },
  ] as const;
  const value = (number: number) => usageAvailable ? formatNumber(number, i18n.language) : "—";

  return (
    <div className="space-y-5">
      <section className={cn("flex items-start gap-3 rounded-xl border p-4", result === "failed" ? "border-red-500/20 bg-red-500/5" : "border-border bg-muted/20")}>
        <span className={cn("flex size-9 shrink-0 items-center justify-center rounded-full", result === "failed" ? "bg-red-500/10 text-red-700 dark:text-red-300" : result === "success" ? "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300" : "bg-muted text-muted-foreground")}><Icon className="size-4.5" /></span>
        <div className="min-w-0 flex-1">
          <h3 className="text-sm font-medium">{t("audits.resultHeadings." + result)}</h3>
          <p className="mt-1 text-xs leading-5 text-muted-foreground">{summary}</p>
          {audit.errorCode ? <div className="mt-2 flex min-w-0 items-start gap-1.5"><code className="break-all rounded bg-red-500/5 px-2 py-1 text-xs text-red-700 dark:text-red-300">{audit.errorCode}</code><CopyButton value={audit.errorCode} copyLabel={t("audits.copyErrorCode")} className="size-7" /></div> : null}
          {result === "failed" || audit.attemptCount > 0 ? <Button variant="link" className="mt-1 h-auto p-0 text-xs" disabled={!diagnosticsAvailable} onClick={onDiagnostics}>{t("audits.inspectFailure")}<ChevronRight className="size-3.5" /></Button> : null}
        </div>
      </section>

      {audit.qualityFailOpen || audit.qualityExempt || (audit.qualityRule && audit.qualityRule !== "thinking") ? <section className="flex items-start gap-2.5 rounded-lg border border-amber-500/20 bg-amber-500/5 px-4 py-3 text-xs leading-5">
        <ShieldAlert className="mt-0.5 size-4 shrink-0 text-amber-700 dark:text-amber-300" />
        <div className="min-w-0 space-y-1">
          {audit.qualityFailOpen ? <p>{t("audits.qualityFailOpenValue")}</p> : null}
          {audit.qualityExempt ? <p><span className="text-muted-foreground">{t("audits.qualityExemptLabel")} · </span>{t("settings.guardStats.exempts." + audit.qualityExempt, { defaultValue: audit.qualityExempt })}</p> : null}
          {audit.qualityRule ? <p><span className="text-muted-foreground">{t("audits.qualityRuleLabel")} · </span>{t("audits.qualityRules." + audit.qualityRule, { defaultValue: audit.qualityRule })}</p> : null}
        </div>
      </section> : null}

      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <DetailMetric icon={<Clock3 />} label={t("audits.durationMetric")} value={formatDuration(audit.durationMs)} />
        <DetailMetric icon={<Zap />} label={t("audits.firstTokenMs")} value={audit.firstTokenMs === undefined ? "—" : formatDuration(audit.firstTokenMs)} />
        <DetailMetric icon={<Activity />} label={t("audits.totalTokens")} value={value(audit.totalTokens)} />
        <DetailMetric icon={<CircleDollarSign />} label={t("audits.billingConclusion")} value={billing ? formatUSDTicks(billing.totalInUsdTicks, 6) : t("audits.unbilled")} />
      </div>

      {billing ? <BillingDetails billing={billing} /> : null}

      <section className="rounded-xl border">
        <div className="flex items-center gap-2 border-b px-4 py-3"><Network className="size-4 text-muted-foreground" /><h3 className="text-sm font-medium">{t("audits.requestRoute")}</h3></div>
        <div className="grid grid-cols-2 gap-x-6 gap-y-4 p-4 lg:grid-cols-3">
          <OverviewField label={t("audits.upstreamModel")} value={audit.modelUpstreamModel || "—"} />
          <OverviewField label={t("audits.targetAccount")} value={audit.accountName || (audit.accountId ? "#" + audit.accountId : "—")} />
          <OverviewField label={t("audits.clientApiKey")} value={audit.clientKeyName || (audit.clientKeyId ? "#" + audit.clientKeyId : "—")} />
          <OverviewField label={t("audits.egressNode")} value={audit.egressMode === "direct" ? t("audits.egressDirect") : audit.egressNodeName || (audit.egressNodeId ? "#" + audit.egressNodeId : "—")} />
          <OverviewField label={t("audits.clientIp")} value={audit.clientIp || "—"} />
          <OverviewField label={t("audits.channelProtocol")} value={providerName(audit.provider) + " · " + t("audits.operations." + audit.operation)} />
          {audit.upstreamStatusCode ? <OverviewField label={t("audits.upstreamStatusCode")} value={String(audit.upstreamStatusCode)} /> : null}
          {audit.responseId ? <OverviewField className="col-span-2" label={t("audits.responseIdentity")} value={audit.responseId} copy /> : null}
        </div>
      </section>

      <section className="rounded-xl border">
        <div className="flex items-center gap-2 border-b px-4 py-3"><ListChecks className="size-4 text-muted-foreground" /><h3 className="text-sm font-medium">{t("audits.executionStages")}</h3></div>
        <div className="grid gap-5 p-4 lg:grid-cols-3">
          {completionGroups.map((group) => <div key={group.title} className="min-w-0">
            <h4 className="mb-3 text-xs font-medium text-muted-foreground">{t("audits." + group.title)}</h4>
            <dl className="space-y-3">{group.stages.map((stage) => <CompletionFact key={stage} label={t("audits.completionStages." + stage)} outcome={audit[stage]} />)}</dl>
          </div>)}
        </div>
        <p className="border-t bg-muted/15 px-4 py-3 text-xs leading-5 text-muted-foreground">{t("audits.completionFacts")}</p>
      </section>

      <section className="rounded-xl border">
        <div className="flex items-center gap-2 border-b px-4 py-3"><Activity className="size-4 text-muted-foreground" /><h3 className="text-sm font-medium">{t("audits.usageAndDelivery")}</h3></div>
        <div className="grid grid-cols-2 gap-x-6 gap-y-4 p-4 sm:grid-cols-4">
          <OverviewField label={t("audits.input")} value={value(audit.inputTokens)} />
          <OverviewField label={t("audits.output")} value={value(audit.outputTokens)} />
          <OverviewField label={t("audits.cached")} value={value(audit.cachedInputTokens)} />
          <OverviewField label={t("audits.reasoning")} value={value(audit.reasoningTokens)} />
          <OverviewField label={t("audits.throughputMetric")} value={audit.outputTokensPerSecond === undefined ? "—" : formatNumber(audit.outputTokensPerSecond, i18n.language, 1) + " " + t("audits.tokensPerSecondUnit")} />
          {audit.reasoningEffort ? <OverviewField label={t("audits.reasoningEffort")} value={t("audits.reasoningEfforts." + audit.reasoningEffort)} /> : null}
          {audit.audioDurationMs !== undefined && audit.audioDurationMs > 0 ? <OverviewField label={t("audits.audioDuration")} value={t("audits.secondsCount", { count: audit.audioDurationMs / 1000 })} /> : null}
          {audit.mediaInputImages > 0 ? <OverviewField label={t("audits.mediaInput")} value={t("audits.imageCount", { count: audit.mediaInputImages })} /> : null}
          {audit.mediaOutputImages > 0 ? <OverviewField label={t("audits.mediaOutput")} value={t("audits.imageCount", { count: audit.mediaOutputImages })} /> : null}
          {audit.mediaOutputSeconds > 0 ? <OverviewField label={t("audits.mediaOutput")} value={t("audits.secondsCount", { count: audit.mediaOutputSeconds })} /> : null}
          {audit.numSourcesUsed > 0 ? <OverviewField label={t("audits.sourcesLabel")} value={formatNumber(audit.numSourcesUsed, i18n.language)} /> : null}
          {audit.numServerSideToolsUsed > 0 ? <OverviewField label={t("audits.generationToolsCount")} value={formatNumber(audit.numServerSideToolsUsed, i18n.language)} /> : null}
        </div>
        <div className="space-y-1 border-t bg-muted/15 px-4 py-3 text-xs leading-5 text-muted-foreground">
          <p>{t("audits.deliveryFacts", { bytes: formatNumber(audit.deliveredBytes, i18n.language), events: formatNumber(audit.deliveredEvents, i18n.language) })}</p>
          {audit.usageSource === "upstream" ? <p>{t("audits.reasoningClaimOnly")}</p> : null}
          {audit.qualityRule === "thinking" ? <p>{t("audits.qualityRuleLabel")} · {t("audits.qualityRules.thinking")}</p> : null}
        </div>
      </section>
      <p className="px-1 text-xs leading-5 text-muted-foreground">{t("audits.description")}</p>
    </div>
  );
}

function BillingDetails({ billing }: { billing: AuditBillingBreakdownDTO }) {
  const { t, i18n } = useTranslation();
  return (
    <details className="group/billing rounded-xl border">
      <summary className="flex cursor-pointer list-none items-center gap-2 rounded-xl px-4 py-3 text-sm font-medium outline-none focus-visible:ring-2 focus-visible:ring-ring [&::-webkit-details-marker]:hidden">
        <CircleDollarSign className="size-4 text-muted-foreground" />{t("audits.billingDetails")}
        <ChevronDown className="ml-auto size-4 text-muted-foreground group-open/billing:rotate-180" />
      </summary>
      <div className="space-y-3 border-t px-4 py-3 text-xs leading-5">
        <div className="grid grid-cols-2 gap-3">
          <OverviewField label={t("audits.billingSource")} value={t(billing.source === "upstream" ? "audits.billingSourceUpstream" : "audits.billingSourceOfficial")} />
          {billing.model ? <OverviewField label={t("audits.billingModel")} value={billing.model} /> : null}
          {billing.version ? <OverviewField label={t("audits.billingVersion")} value={billing.version} /> : null}
          {billing.tier === "long_context" ? <OverviewField label={t("audits.billingRateTier")} value={t("audits.billingLongContextTier")} /> : null}
        </div>
        <div className="space-y-1 border-t pt-3">
          <p className="mb-2 text-muted-foreground">{t("audits.billingFormula")}</p>
          {billing.method === "upstream_reported" ? <p>{t("audits.billingUpstreamFormula")}</p>
            : billing.method === "stored_estimate" ? <p>{t("audits.billingStoredFormulaUnavailable")}</p>
            : billing.components.length === 0 ? <p>{t("audits.billingZeroFormula")}</p>
            : billing.components.map((component) => <BillingFormula key={component.kind} component={component} locale={i18n.language} />)}
        </div>
        <div className="flex flex-wrap items-center justify-between gap-2 border-t pt-3 font-medium"><span>{t("audits.billingConclusion")}</span><span className="font-mono tabular-nums">{formatUSDTicks(billing.totalInUsdTicks, 10)}</span></div>
      </div>
    </details>
  );
}

function BillingFormula({ component, locale }: { component: AuditBillingComponentDTO; locale: string }) {
  const { t } = useTranslation();
  const quantity = formatNumber(component.quantity, locale, 0);
  const unitCost = (ticks: number) => `$${usdTicksToValue(ticks).toFixed(10).replace(/0+$/, "").replace(/\.$/, "")}`;
  const formula = component.unit === "token"
    ? `${quantity} / 1M × ${unitCost(component.unitPriceInUsdTicks * 1_000_000)}`
    : `${quantity} × ${unitCost(component.unitPriceInUsdTicks)} / ${t(`audits.billingUnits.${component.unit}`)}`;
  return <div className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3"><span className="text-muted-foreground">{t(`audits.billingComponents.${component.kind}`)}</span><span className="break-words text-right font-mono tabular-nums">{formula} = {formatUSDTicks(component.subtotalInUsdTicks, 10)}</span></div>;
}

function DetailMetric({ icon, label, value }: { icon: ReactNode; label: string; value: string }) {
  return <div className="min-w-0 rounded-xl border bg-muted/15 p-4">
    <div className="flex items-center gap-1.5 text-xs text-muted-foreground [&_svg]:size-3.5">{icon}{label}</div>
    <p className="mt-2 break-words text-xl font-medium tracking-tight tabular-nums">{value}</p>
  </div>;
}

function CompletionFact({ label, outcome }: { label: string; outcome?: string }) {
  const { t } = useTranslation();
  const tone = completionTone(outcome);
  const Icon = tone === "success" ? Check : tone === "failed" ? X : tone === "warning" ? TriangleAlert : Minus;
  return <div className="flex items-start justify-between gap-3 text-xs">
    <dt className="min-w-0 leading-5">{label}</dt>
    <dd className={cn("flex shrink-0 items-center gap-1 leading-5", tone === "success" ? "text-emerald-700 dark:text-emerald-300" : tone === "failed" ? "text-red-700 dark:text-red-300" : tone === "warning" ? "text-amber-700 dark:text-amber-300" : "text-muted-foreground")}>
      <Icon className="size-3" />{t("audits.completionOutcomes." + (outcome || "not_recorded"), { defaultValue: outcome })}
    </dd>
  </div>;
}

function providerName(provider: AuditDTO["provider"]): string {
  return { grok_build: "Grok Build", grok_web: "Grok Web", grok_console: "Grok Console" }[provider];
}

function RequestMetadataPanel({ audit }: { audit: AuditDTO }) {
  const { t } = useTranslation();
  const headers = useMemo(() => audit.requestHeaders ?? {}, [audit.requestHeaders]);

  if (!audit.requestMethod && !audit.requestPath && Object.keys(headers).length === 0) {
    return <EmptyPanel icon={<FileText />} message={t("audits.noRequestMetadata")} />;
  }

  return (
    <div className="flex h-full min-h-0 flex-col gap-4">
      <section className="shrink-0">
        <p className="mb-2 px-1 text-[11px] font-medium text-muted-foreground">{t("audits.requestPath")}</p>
        <div className="flex h-10 min-w-0 items-center gap-3 rounded-lg bg-muted/15 px-3">
          <span className="shrink-0 text-xs text-muted-foreground">
            {audit.requestMethod || "-"}
          </span>
          <span className="min-w-0 flex-1 truncate text-xs" title={audit.requestPath}>
            {audit.requestPath || "-"}
          </span>
          {audit.requestPath ? <CopyButton value={audit.requestPath} /> : null}
        </div>
      </section>
      <section className="min-h-0 flex-1">
        <HeadersPanel title={t("audits.requestHeaders")} headers={headers} emptyMessage={t("audits.noRequestHeaders")} />
      </section>
    </div>
  );
}

function UpstreamAttemptsPanel({
  audit,
  attempts,
}: {
  audit: AuditDTO;
  attempts: AuditAttemptDTO[];
}) {
  const { t } = useTranslation();
  const [selectedNumber, setSelectedNumber] = useState<number | null>(null);
  const attemptListRef = useRef<HTMLDivElement>(null);

  const orderedAttempts = useMemo(() => [...attempts].sort((left, right) => left.number - right.number), [attempts]);
  const selectedAttempt = orderedAttempts.find((attempt) => attempt.number === selectedNumber) ?? orderedAttempts[orderedAttempts.length - 1];
  useEffect(() => {
    const list = attemptListRef.current;
    const selected = list?.querySelector<HTMLElement>('[aria-pressed="true"]');
    if (list && selected && list.scrollWidth > list.clientWidth) list.scrollLeft = selected.offsetLeft;
  }, [selectedAttempt?.id]);

  if (attempts.length === 0) {
    const isSuccess = audit.statusCode >= 200 && audit.statusCode < 300 && !audit.errorCode;
    if (isSuccess) {
      return (
        <div className="flex h-full min-h-0 flex-col items-center justify-center gap-2 p-6 text-center text-muted-foreground">
          <CheckCircle2 className="size-8 stroke-1 text-emerald-500" />
          <p className="text-xs">{t("audits.successNoAttempts")}</p>
        </div>
      );
    }
    return (
      <div className="flex h-full min-h-0 flex-col items-center justify-center gap-2 p-6 text-center text-muted-foreground">
        <TriangleAlert className="size-8 stroke-1 text-amber-500" />
        <p className="max-w-md text-xs">
          {t(
            audit.errorCode && PRE_UPSTREAM_ERROR_CODES.has(audit.errorCode)
              ? "audits.noUpstreamAttempt"
              : "audits.noFailureAttempts"
          )}
        </p>
        {audit.errorCode ? (
          <Badge variant="outline" className="font-mono text-xs">
            {audit.errorCode}
          </Badge>
        ) : null}
      </div>
    );
  }

  const terminalAttemptNumber = orderedAttempts[orderedAttempts.length - 1].number;

  return (
    <div className="grid h-full min-h-0 flex-1 grid-rows-[auto_minmax(0,1fr)] lg:grid-cols-[232px_minmax(0,1fr)] lg:grid-rows-1">
      <aside className="flex min-h-0 min-w-0 flex-col overflow-hidden border-b bg-muted/25 p-4 lg:border-r lg:border-b-0">
        <p className="mb-3 shrink-0 text-xs font-medium text-muted-foreground">{t("audits.attemptTimeline")}</p>
        <div ref={attemptListRef} className="relative flex max-h-32 gap-2 overflow-auto lg:min-h-0 lg:max-h-none lg:flex-1 lg:flex-col">
          {orderedAttempts.map((attempt) => (
            <AttemptButton
              key={attempt.id}
              attempt={attempt}
              latest={attempt.number === terminalAttemptNumber}
              selected={attempt.number === selectedAttempt.number}
              onClick={() => setSelectedNumber(attempt.number)}
            />
          ))}
        </div>
      </aside>
      <AttemptDetail key={selectedAttempt.id} attempt={selectedAttempt} />
    </div>
  );
}

function AttemptButton({ attempt, latest, selected, onClick }: { attempt: AuditAttemptDTO; latest: boolean; selected: boolean; onClick: () => void }) {
  const { t } = useTranslation();
  const quality = attempt.stage === "quality_hold" || attempt.stage === "quality_idle";
  const Icon = quality ? ShieldAlert : attempt.source === "upstream_http" ? Server : attempt.source === "gateway_transport" ? Network : KeyRound;
  return (
    <button type="button" className={cn(
      "w-48 shrink-0 space-y-2 rounded-lg border p-3 text-left text-xs outline-none transition-colors focus-visible:ring-2 focus-visible:ring-foreground/30 lg:w-full",
      selected ? "border-foreground/25 bg-background shadow-sm" : "border-transparent hover:bg-background/60",
    )} aria-pressed={selected} onClick={onClick}>
      <span className="flex items-center justify-between gap-2">
        <span className="font-medium">{t("audits.attemptNumber", { number: attempt.number })}</span>
        {latest ? <span className="text-[10px] text-muted-foreground">{t("audits.latestAttempt")}</span> : null}
      </span>
      <span className="block truncate text-muted-foreground">{attempt.accountName || (attempt.accountId ? "#" + attempt.accountId : "—")}</span>
      <span className="flex items-center gap-1.5 text-[11px] text-muted-foreground">
        <Icon className={cn("size-3.5", quality ? "text-amber-700 dark:text-amber-300" : "text-red-700 dark:text-red-300")} />
        {attempt.upstreamStatusCode ? "HTTP " + attempt.upstreamStatusCode : t("audits.noHttpResponse")}
        <span className="ml-auto tabular-nums">{formatDuration(attempt.durationMs)}</span>
      </span>
    </button>
  );
}

function AttemptDetail({ attempt }: { attempt: AuditAttemptDTO }) {
  const { t } = useTranslation();
  const hasBody = Boolean(attempt.responseBody);
  return (
    <div className="min-h-0 min-w-0 flex-1 space-y-5 overflow-y-auto overscroll-contain p-4 sm:p-6">
      <header className="space-y-2 border-b pb-4">
        <AttemptSummary attempt={attempt} />
        <p className="text-xs leading-5 text-muted-foreground">{t(attempt.upstreamStatusCode ? "audits.upstreamResponseReceived" : "audits.noUpstreamResponse")}</p>
      </header>
      {attempt.transportError ? <div className="flex items-start gap-2 rounded-lg border border-red-500/20 bg-red-500/5 p-3">
        <p className="min-w-0 flex-1 break-words font-mono text-xs leading-5 text-red-700 dark:text-red-300">{attempt.transportError}</p>
        <CopyButton value={attempt.transportError} />
      </div> : null}
      <AttemptOverview attempt={attempt} />
      <div className="space-y-3">
        <DiagnosticDisclosure title={t("audits.responseBody")} initiallyOpen={hasBody}>
          <div className="h-64"><AttemptResponseBody attempt={attempt} /></div>
        </DiagnosticDisclosure>
        <DiagnosticDisclosure title={t("audits.errorChain")} count={attempt.errorChain.length} initiallyOpen={!hasBody && attempt.errorChain.length > 0}>
          <div className="h-64"><ErrorChainPanel attempt={attempt} /></div>
        </DiagnosticDisclosure>
        <DiagnosticDisclosure title={t("audits.responseHeaders")} count={Object.keys(attempt.responseHeaders).length}>
          <div className="h-64"><HeadersPanel headers={attempt.responseHeaders} /></div>
        </DiagnosticDisclosure>
      </div>
    </div>
  );
}

function DiagnosticDisclosure({ title, count, initiallyOpen = false, children }: { title: string; count?: number; initiallyOpen?: boolean; children: ReactNode }) {
  return <details open={initiallyOpen} className="group/diagnostic overflow-hidden rounded-lg border">
    <summary className="flex cursor-pointer items-center gap-2 px-4 py-3 text-xs font-medium outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-foreground/30">
      <ChevronDown className="size-3.5 -rotate-90 text-muted-foreground transition-transform group-open/diagnostic:rotate-0" />
      {title}{count !== undefined ? <span className="ml-auto rounded bg-muted px-1.5 py-0.5 text-[10px] tabular-nums text-muted-foreground">{count}</span> : null}
    </summary>
    <div className="border-t">{children}</div>
  </details>;
}

function AttemptResponseBody({ attempt }: { attempt: AuditAttemptDTO }) {
  const { t } = useTranslation();
  const displayValue = useMemo(() => formattedResponseBody(attempt), [attempt]);
  return (
    <CodePanel
      value={attempt.responseBody}
      displayValue={displayValue}
      emptyMessage={t("audits.emptyResponseBody")}
      encoding={attempt.responseBodyEncoding}
      truncated={attempt.responseBodyTruncated}
    />
  );
}

function AttemptSummary({ attempt }: { attempt: AuditAttemptDTO }) {
  const { t } = useTranslation();
  const isHTTP = attempt.source === "upstream_http";
  const isStreamFailure = isHTTP && attempt.stage === "response_stream";
  // 质量守卫的尝试不是上游失败:上游 HTTP 是 200,是网关主动扣留/中止。
  // 误标成“上游 HTTP 失败”会掩盖“哪一尝试被守卫拦下”这条关键轨迹。
  const isQualityHold = isHTTP && attempt.stage === "quality_hold";
  const isQualityIdle = isHTTP && attempt.stage === "quality_idle";
  const Icon = isQualityHold || isQualityIdle ? ShieldAlert : isHTTP ? Server : attempt.source === "gateway_transport" ? Network : KeyRound;
  const title = isQualityHold
    ? t("audits.qualityHoldAttempt", { status: attempt.upstreamStatusCode ?? "-" })
    : isQualityIdle
    ? t("audits.qualityIdleAttempt", { status: attempt.upstreamStatusCode ?? "-" })
    : isStreamFailure
    ? t("audits.upstreamStreamFailure", { status: attempt.upstreamStatusCode ?? "-" })
    : isHTTP
    ? t("audits.upstreamHttpFailure", { status: attempt.upstreamStatusCode ?? "-" })
    : attempt.source === "gateway_transport"
    ? t("audits.gatewayTransportFailure")
    : t("audits.credentialFailure");
  return (
    <div className="flex min-w-0 items-center gap-2">
      <Icon className={cn("size-5 shrink-0", isQualityHold || isQualityIdle ? "text-amber-700 dark:text-amber-300" : "text-red-700 dark:text-red-300")} />
      <h3 className="min-w-0 text-sm font-medium leading-6">{title}</h3>
    </div>
  );
}

function AttemptOverview({ attempt }: { attempt: AuditAttemptDTO }) {
  const { t, i18n } = useTranslation();
  return (
    <div className="grid grid-cols-2 gap-x-6 gap-y-4">
      <OverviewField label={t("audits.attemptStartedAt")} value={formatDateTime(attempt.startedAt, i18n.language)} />
      <OverviewField label={t("audits.duration")} value={`${formatNumber(attempt.durationMs, i18n.language)} ms`} />
      <OverviewField label={t("audits.targetAccount")} value={attempt.accountName || (attempt.accountId ? `#${attempt.accountId}` : "-")} />
      <OverviewField label={t("audits.requestMethod")} value={attempt.method || "-"} />
      <OverviewField label={t("audits.requestPath")} value={attempt.requestPath || "-"} />
      <OverviewField label={t("audits.upstreamStatus")} value={attempt.upstreamStatus || (attempt.upstreamStatusCode ? String(attempt.upstreamStatusCode) : "-")} />
      <OverviewField className="col-span-2" label={t("audits.upstreamUrl")} value={attempt.upstreamUrl || t("audits.upstreamUrlUnavailable")} copy={Boolean(attempt.upstreamUrl)} />

    </div>
  );
}

function OverviewField({ className, label, value, copy }: { className?: string; label: string; value: string; copy?: boolean }) {
  return (
    <div className={cn("flex min-w-0 items-start gap-3", className)}>
      <div className="min-w-0 flex-1">
        <p className="text-[11px] text-muted-foreground">{label}</p>
        <p className="mt-1 break-all text-xs font-medium leading-5" title={value}>
          {value}
        </p>
      </div>
      {copy ? (
        <div className="shrink-0 pt-0.5">
          <CopyButton value={value} />
        </div>
      ) : null}
    </div>
  );
}

function CodePanel({
  value,
  displayValue,
  emptyMessage,
  encoding,
  truncated,
}: {
  value: string;
  displayValue: string;
  emptyMessage: string;
  encoding: string;
  truncated: boolean;
}) {
  const { t } = useTranslation();
  if (!value) return <EmptyPanel icon={<FileText />} message={emptyMessage} />;
  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden bg-muted/20">
      <div className="flex h-10 shrink-0 items-center justify-between px-3">
        <span className="flex min-w-0 items-center gap-2 text-muted-foreground text-[11px]">
          <span>{t("audits.bodyEncoding", { encoding })}</span>
          {truncated ? <Badge variant="outline" className="text-[10px]">{t("audits.bodyTruncated")}</Badge> : null}
        </span>
        <CopyButton value={value} />
      </div>
      <div className="min-h-0 flex-1 overflow-auto p-3">
        <pre className="font-mono text-[11px] leading-relaxed whitespace-pre-wrap break-all select-text">
          {displayValue}
        </pre>
      </div>
    </div>
  );
}

function HeadersPanel({ title, headers, emptyMessage }: { title?: string; headers: Record<string, string[]>; emptyMessage?: string }) {
  const { t } = useTranslation();
  const entries = useMemo(() => Object.entries(headers).sort(([left], [right]) => left.localeCompare(right)), [headers]);
  const copyValue = useMemo(() => JSON.stringify(headers, null, 2), [headers]);
  if (entries.length === 0) return <EmptyPanel icon={<FileText />} message={emptyMessage ?? t("audits.emptyResponseHeaders")} />;
  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden bg-muted/15">
      <div className="flex h-10 shrink-0 items-center justify-between px-3">
        <span className="flex min-w-0 items-center gap-2 text-[11px]">
          {title ? <span className="truncate font-medium text-foreground">{title}</span> : null}
          <span className="text-muted-foreground">{t("audits.headerItemCount", { count: entries.length })}</span>
        </span>
        <CopyButton value={copyValue} />
      </div>
      <div className="min-h-0 flex-1 space-y-0.5 overflow-auto px-2 pb-2">
        {entries.map(([name, values], entryIndex) => (
          <div key={name} className={cn("grid gap-1 rounded-md px-2.5 py-2 transition-colors hover:bg-background/70 sm:grid-cols-[180px_minmax(0,1fr)] sm:gap-4", entryIndex % 2 === 0 && "bg-background/35")}>
            <span className="break-all font-mono text-[11px] text-muted-foreground">{name}</span>
            <div className="min-w-0 space-y-1">
              {values.map((value, index) => (
                <span key={`${name}-${index}`} className={cn("block break-all font-mono text-[11px]", value === "[REDACTED]" && "font-medium text-amber-700 dark:text-amber-300")}>{value}</span>
              ))}
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

function ErrorChainPanel({ attempt }: { attempt: AuditAttemptDTO }) {
  const { t } = useTranslation();
  const copyValue = useMemo(() => JSON.stringify(attempt.errorChain, null, 2), [attempt.errorChain]);
  if (attempt.errorChain.length === 0) return <EmptyPanel icon={<Network />} message={t("audits.emptyErrorChain")} />;
  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden bg-muted/15">
      <div className="flex h-10 shrink-0 items-center justify-between px-3">
        <span className="text-muted-foreground text-[11px]">{t("audits.errorFrameCount", { count: attempt.errorChain.length })}</span>
        <CopyButton value={copyValue} />
      </div>
      <ol className="min-h-0 flex-1 space-y-3 overflow-auto p-3">
        {attempt.errorChain.map((frame, index) => (
          <li key={`${frame.type}-${index}`} className="rounded-md bg-background/50 p-2.5">
            <div className="flex items-center gap-2 text-muted-foreground text-[11px]">
              <span>#{index + 1}</span>
              <span className="break-all font-medium text-foreground">{frame.type}</span>
            </div>
            <p className="mt-1.5 font-mono text-[11px] whitespace-pre-wrap break-words">{frame.message}</p>
          </li>
        ))}
      </ol>
    </div>
  );
}

function EmptyPanel({ icon, message }: { icon: ReactNode; message: string }) {
  return (
    <div className="flex h-full min-h-40 flex-col items-center justify-center gap-2 rounded-lg bg-muted/15 px-6 text-center text-muted-foreground [&_svg]:size-6 [&_svg]:stroke-1">
      <span>{icon}</span>
      <p className="text-xs">{message}</p>
    </div>
  );
}

function formattedResponseBody(attempt: AuditAttemptDTO): string {
  if (attempt.responseBodyEncoding !== "utf8") return attempt.responseBody;
  const contentType = Object.entries(attempt.responseHeaders).find(([name]) => name.toLowerCase() === "content-type")?.[1].join(";") ?? "";
  if (attempt.stage !== "response_stream" && !contentType.toLowerCase().includes("json")) return attempt.responseBody;
  return formatJSONBody(attempt.responseBody);
}

function formatJSONBody(value: string): string {
  try {
    return JSON.stringify(JSON.parse(value), null, 2);
  } catch {
    return value;
  }
}
