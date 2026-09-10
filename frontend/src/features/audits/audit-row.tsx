import { ChevronRight, KeyRound, Minimize2, Monitor, UserRound, Waypoints } from "lucide-react";
import { memo } from "react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { TableCell, TableRow } from "@/components/ui/table";
import { formatCompactDateTime, formatDuration, formatNumber } from "@/shared/lib/format";
import { formatUSDTicks } from "@/shared/lib/usd";
import { auditBilling, auditOutcome, auditProviderLabel } from "./audit-presentation";
import { AuditResultButton } from "./audit-result-mark";
import { buildAuditUsageView } from "./audit-usage";
import type { AuditDTO } from "./request-audits-api";

type RowProps = { audit: AuditDTO; locale: string; onOpen: (audit: AuditDTO) => void };

export const AuditRow = memo(function AuditRow({ audit, locale, onOpen }: RowProps) {
  return <TableRow className="audit-data-row" data-outcome={auditOutcome(audit)}>
    <TableCell className="audit-status-column"><AuditResultButton audit={audit} onOpen={() => onOpen(audit)} /></TableCell>
    <TableCell><RequestIdentity audit={audit} onOpen={() => onOpen(audit)} /></TableCell>
    <TableCell><RequestConnection audit={audit} /></TableCell>
    <TableCell><ResponsePerformance audit={audit} locale={locale} /></TableCell>
    <TableCell><RequestUsage audit={audit} locale={locale} /></TableCell>
    <TableCell><RequestTime audit={audit} locale={locale} /></TableCell>
  </TableRow>;
});

export const AuditMobileCard = memo(function AuditMobileCard({ audit, locale, onOpen }: RowProps) {
  const { t } = useTranslation();
  return <article className="audit-mobile-card" data-outcome={auditOutcome(audit)}>
    <div className="audit-mobile-heading"><RequestIdentity audit={audit} onOpen={() => onOpen(audit)} /><AuditResultButton audit={audit} onOpen={() => onOpen(audit)} /></div>
    <div className="audit-mobile-route"><RequestConnection audit={audit} /></div>
    <div className="audit-mobile-metrics"><ResponsePerformance audit={audit} locale={locale} /><RequestUsage audit={audit} locale={locale} /></div>
    <div className="audit-mobile-footer"><RequestTime audit={audit} locale={locale} /><Button variant="ghost" size="sm" onClick={() => onOpen(audit)} aria-label={t("audits.openRequest", { id: audit.requestId })}>{t("audits.viewDetails")}<ChevronRight className="size-3.5" /></Button></div>
  </article>;
});

function RequestIdentity({ audit, onOpen }: { audit: AuditDTO; onOpen: () => void }) {
  const { t } = useTranslation();
  const model = audit.modelPublicId || "#" + audit.modelRouteId;
  return <div className="audit-identity">
    <div className="audit-model-heading">
      <button type="button" className="audit-model-link" aria-label={t("audits.openModel", { model })} onClick={onOpen}>{model}</button>
      {audit.reasoningEffort ? <span className="audit-reasoning-effort" data-effort={audit.reasoningEffort} aria-label={t("audits.reasoningEffort") + ": " + audit.reasoningEffort}>{audit.reasoningEffort}</span> : null}
    </div>
    <div className="audit-channel"><span className="audit-provider" data-provider={audit.provider}>{auditProviderLabel(audit.provider)}</span><span className="audit-operation">{t("audits.operations." + audit.operation)}</span><span className="audit-stream-mode" data-streaming={audit.streaming}>{t(audit.streaming ? "audits.stream" : "audits.nonStream")}</span></div>
    <div className="audit-account"><UserRound aria-hidden="true" /><span>{audit.accountName || (audit.accountId ? "#" + audit.accountId : "—")}</span></div>
  </div>;
}

function RequestConnection({ audit }: { audit: AuditDTO }) {
  const { t } = useTranslation();
  const node = audit.egressNodeName || (audit.egressNodeId ? "#" + audit.egressNodeId : t("audits.egressUnknown"));
  const exit = audit.egressMode === "proxy" ? node : audit.egressMode === "direct" ? t("audits.egressDirect") : t("audits.egressNotRecorded");
  return <div className="audit-connection">
    <RequestClientKey audit={audit} />
    <div className="audit-connection-side" data-side="source" aria-label={t("audits.connectionSource")}>
      <div className="audit-connection-primary"><Monitor aria-hidden="true" /><span className="audit-source-ip">{audit.clientIp || "—"}</span></div>
    </div>
    <div className="audit-connection-side" data-side="egress" data-mode={audit.egressMode || "unknown"} aria-label={t("audits.egressRoute")}>
      <div className="audit-connection-primary"><Waypoints aria-hidden="true" /><span className="audit-connection-node">{exit}</span></div>
    </div>
  </div>;
}

function RequestClientKey({ audit }: { audit: AuditDTO }) {
  const { t } = useTranslation();
  return <div className="audit-client-key" aria-label={t("audits.apiKeyColumn")}><KeyRound aria-hidden="true" /><span>{audit.clientKeyName || (audit.clientKeyId && audit.clientKeyId !== "0" ? "#" + audit.clientKeyId : "—")}</span></div>;
}

function ResponsePerformance({ audit, locale }: { audit: AuditDTO; locale: string }) {
  const { t } = useTranslation();
  return <div className="audit-performance">
    <strong>{formatDuration(audit.durationMs)}</strong>
    <div><span>{t("audits.firstTokenMetric")}</span><span>{audit.firstTokenMs === undefined ? "—" : formatDuration(audit.firstTokenMs)}</span></div>
    <p>{audit.outputTokensPerSecond === undefined ? audit.degradeClass === "terminal_burst" ? t("audits.degradeClassTerminalBurst") : "— " + t("audits.tokensPerSecondUnit") : formatNumber(audit.outputTokensPerSecond, locale, 1) + " " + t("audits.tokensPerSecondUnit")}</p>
  </div>;
}

function RequestUsage({ audit, locale }: { audit: AuditDTO; locale: string }) {
  const { t } = useTranslation();
  const billing = auditBilling(audit);
  const view = buildAuditUsageView(audit, (value) => value >= 1_000_000 ? formatNumber(value / 1_000_000, locale, 2) + "M" : value >= 10_000 ? formatNumber(value / 1_000, locale, 1) + "k" : formatNumber(value, locale), {
    input: t("audits.input"), output: t("audits.output"), cached: t("audits.cached"), reasoning: t("audits.reasoning"),
    mediaInput: t("audits.mediaInput"), mediaOutput: t("audits.mediaOutput"), imageCount: (count) => t("audits.imageCount", { count }), secondsCount: (count) => t("audits.secondsCount", { count }),
  });
  return <div className="audit-usage">
    {view.mode === "compaction" ? <div className="audit-special-usage"><Minimize2 aria-hidden="true" /><span>{t("audits.compactionUsageUnavailable")}</span></div> : view.mode === "duration" ? <div className="audit-special-usage"><span>{t("audits.operations." + audit.operation)}</span><strong>{view.durationSeconds}s</strong></div> : <dl className="audit-usage-grid">
      {[...view.mediaItems ?? [], ...view.tokenItems ?? []].map((item) => <div key={item.key} data-usage={item.key}><dt>{item.label}</dt><dd>{item.value}</dd></div>)}
    </dl>}
    <div className="audit-cost-line"><span>{billing ? formatUSDTicks(billing.totalInUsdTicks, 4) : t("audits.unbilled")}</span>{audit.numServerSideToolsUsed > 0 ? <small>{t("audits.serverTools", { count: audit.numServerSideToolsUsed })}</small> : null}</div>
  </div>;
}

function RequestTime({ audit, locale }: { audit: AuditDTO; locale: string }) {
  const [date, time] = formatCompactDateTime(audit.createdAt, locale).split(" ");
  return <time className="audit-time" dateTime={audit.createdAt}><span>{time || "—"}</span><small>{date}</small></time>;
}
