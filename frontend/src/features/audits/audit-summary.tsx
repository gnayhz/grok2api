import { Activity, Check, CircleDollarSign, Clock3, Layers3 } from "lucide-react";
import { memo, type CSSProperties, type ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { formatDuration, formatNumber } from "@/shared/lib/format";
import { formatUSDTicks } from "@/shared/lib/usd";
import type { DashboardDTO } from "@/entities/dashboard/dashboard-api";
import type { AuditSummaryDTO } from "@/entities/audit/audit-api";

export const AuditSummary = memo(function AuditSummary({ summary, loading, updating, filtered, periodStats, periodLoading }: {
  summary?: AuditSummaryDTO;
  loading: boolean;
  updating: boolean;
  filtered: boolean;
  periodStats?: DashboardDTO;
  periodLoading: boolean;
}) {
  const { t, i18n } = useTranslation();
  const usage = summary?.usage;
  const pricing = summary?.pricing;
  const number = (value?: number) => loading || value === undefined ? "—" : formatNumber(value, i18n.language, 0);
  const hasRequests = Boolean(usage?.requests);
  const hasPrice = Boolean(pricing?.pricedRequests);
  const successRate = !loading && hasRequests ? usage!.successRate : undefined;
  const input = usage?.inputTokens ?? 0;
  const output = usage?.outputTokens ?? 0;
  const cacheRate = !loading && input ? (usage?.cachedInputTokens ?? 0) / input * 100 : undefined;
  const inputWidth = input + output > 0 ? input / (input + output) * 100 : 0;
  const cost = usage?.estimatedCostInUsdTicks ?? 0;
  // Both terms come from the same period snapshot. Audit withholds are
  // records (including rejected attempts), not deduplicated logical requests.
  const periodUsage = periodLoading ? undefined : periodStats?.usage;
  const withholds = periodLoading ? undefined : periodStats?.resources.qualityDegradedRequests;
  const interceptRate = periodUsage?.requests && withholds !== undefined ? withholds / periodUsage.requests * 100 : undefined;
  const firstToken = periodUsage?.firstTokenSamples ? periodUsage.averageFirstTokenMs : undefined;
  const throughput = periodUsage?.throughputSamples ? periodUsage.outputTokensPerSecond : undefined;
  const periodScope = filtered ? t("audits.summaryWholePeriod") : undefined;
  const totalDuration = loading || !usage ? undefined : usage.averageDurationMs * usage.requests;
  const durationNumber = (value: number) => formatNumber(value, i18n.language, 0);
  const totalDurationLabel = totalDuration === undefined ? "—" : totalDuration < 60_000
    ? formatDuration(Math.round(totalDuration))
    : totalDuration < 3_600_000
      ? `${durationNumber(Math.floor(totalDuration / 60_000))}m ${durationNumber(Math.floor(totalDuration / 1_000) % 60)}s`
      : totalDuration < 86_400_000
        ? `${durationNumber(Math.floor(totalDuration / 3_600_000))}h ${durationNumber(Math.floor(totalDuration / 60_000) % 60)}m`
        : `${durationNumber(Math.floor(totalDuration / 86_400_000))}d ${durationNumber(Math.floor(totalDuration / 3_600_000) % 24)}h`;

  return (
    <section className="audit-overview" aria-label={t("audits.usageSummary")} aria-busy={loading || updating}>
      <div className="audit-overview-grid">
        <article className="audit-overview-requests" aria-label={t("audits.summaryRequests")}>
          <SummaryHeading icon={<Activity />} label={t("audits.summaryRequests")} scope={filtered ? t("audits.summaryFiltered") : undefined} />
          <div className="audit-card-primary">
            <SummaryValue>{number(usage?.requests)}</SummaryValue>
            <p className="audit-card-caption">
              {loading ? "—" : !hasRequests ? t("audits.summaryNoRequests") : usage?.failedRequests ? t("audits.summaryFailures", { count: usage.failedRequests }) : <><Check />{t("audits.summaryAllSucceeded")}</>}
            </p>
          </div>
          <div className="audit-card-secondary audit-request-rings">
            <MetricRing kind="success" value={successRate} label={t("audits.successRate")} description={t("audits.requestBreakdown", { success: number(usage?.successfulRequests), failed: number(usage?.failedRequests) })} />
            <MetricRing kind="intercept" value={interceptRate} label={t("audits.summaryInterceptRate")} scope={periodScope} description={t("audits.summaryInterceptDescription", { count: withholds ?? "—", total: periodUsage?.requests ?? "—" })} />
          </div>
        </article>

        <article className="audit-overview-latency" aria-label={t("audits.summaryLatency")}>
          <SummaryHeading icon={<Clock3 />} label={t("audits.summaryLatency")} />
          <div className="audit-card-primary-pair">
            <div className="audit-card-primary">
              <SummaryValue>{loading || !hasRequests ? "—" : formatDuration(usage!.averageDurationMs)}</SummaryValue>
              <p className="audit-card-caption">{t("audits.summaryAverageDuration")}</p>
            </div>
            <div className="audit-card-primary audit-primary-aside">
              <span className="audit-aside-value">{totalDurationLabel}</span>
              <p className="audit-card-caption">{t("audits.summaryTotalDuration")}</p>
            </div>
          </div>
          <dl className="audit-card-secondary audit-response-metrics">
            <div><dt>{t("audits.summaryAverageFirstToken")}{periodScope ? <small>{periodScope}</small> : null}</dt><dd>{firstToken === undefined ? "—" : formatDuration(firstToken)}</dd></div>
            <div><dt>{t("audits.summaryThroughput")}{periodScope ? <small>{periodScope}</small> : null}</dt><dd>{throughput === undefined ? "—" : formatNumber(throughput, i18n.language, 1)}<small>t/s</small></dd></div>
          </dl>
        </article>

        <article className="audit-overview-tokens" aria-label={t("audits.summaryTokens")}>
          <SummaryHeading icon={<Layers3 />} label={t("audits.summaryTokens")} />
          <div className="audit-card-primary">
            <SummaryValue>{number(usage?.totalTokens)}</SummaryValue>
            <div className="audit-token-flow" role="img" aria-label={t("audits.summaryTokenFlow", { input: number(input), output: number(output) })}>
              <span className="audit-token-input" style={{ width: `${inputWidth}%` }}><span className="audit-token-cache" style={{ width: `${Math.min(100, Math.max(0, cacheRate ?? 0))}%` }} /></span>
              <span className="audit-token-output" style={{ width: `${input + output > 0 ? 100 - inputWidth : 0}%`, display: output ? undefined : "none" }} />
            </div>
          </div>
          <div className="audit-card-secondary audit-token-details">
            <dl className="audit-token-legend">
              <div><dt><i className="audit-input-dot" />{t("audits.input")}</dt><dd>{number(usage?.inputTokens)}</dd></div>
              <div><dt><i className="audit-output-dot" />{t("audits.output")}</dt><dd>{number(usage?.outputTokens)}</dd></div>
              <div><dt><i className="audit-cache-dot" />{t("audits.cached")}</dt><dd>{number(usage?.cachedInputTokens)}</dd></div>
              <div><dt><i className="audit-reasoning-dot" />{t("audits.reasoning")}</dt><dd>{number(usage?.reasoningTokens)}</dd></div>
            </dl>
            <MetricRing kind="cache" value={cacheRate} label={t("audits.summaryCacheHitRate")} description={t("audits.summaryCacheDescription")} />
          </div>
        </article>

        <article className="audit-overview-cost" aria-label={t("audits.estimatedCost")}>
          <SummaryHeading icon={<CircleDollarSign />} label={t("audits.estimatedCost")} />
          <div className="audit-card-primary">
            <SummaryValue>{loading || !hasPrice ? "—" : formatUSDTicks(cost, 2)}</SummaryValue>
          </div>
          <dl className="audit-card-secondary audit-cost-average">
            <div><dt>{t("audits.summaryAverageCost")}</dt><dd>{loading || !hasPrice || !hasRequests ? "—" : formatUSDTicks(cost / usage!.requests, 4)}</dd></div>
            <div data-warning={!loading && (pricing?.unpricedRequests ?? 0) > 0}><dt>{t("audits.summaryUnpricedRequests")}</dt><dd>{number(pricing?.unpricedRequests)}</dd></div>
          </dl>
        </article>
      </div>
    </section>
  );
});

function SummaryHeading({ icon, label, scope }: { icon: ReactNode; label: string; scope?: string }) {
  return <header className="audit-overview-heading"><h2>{icon}{label}</h2>{scope ? <span className="audit-overview-scope-tag">{scope}</span> : null}</header>;
}

function SummaryValue({ children }: { children: ReactNode }) {
  return <div className="audit-overview-value">{children}</div>;
}

// Interpolate the presentation palette so nearby rates have nearby colors.
function interpolateHue(value: number, stops: number[][]): number {
  for (let index = 1; index < stops.length; index++) {
    const [end, endHue] = stops[index];
    if (value <= end) {
      const [start, startHue] = stops[index - 1];
      return startHue + (endHue - startHue) * (value - start) / (end - start);
    }
  }
  return stops[stops.length - 1][1];
}

function MetricRing({ kind, value, label, description, scope }: {
  kind: "success" | "intercept" | "cache";
  value?: number;
  label: string;
  description: string;
  scope?: string;
}) {
  const { i18n } = useTranslation();
  const bounded = value === undefined ? undefined : Math.min(100, Math.max(0, value));
  const shown = bounded === undefined ? "—" : `${formatNumber(bounded, i18n.language, 1)}%`;
  const hue = bounded === undefined ? 0 : kind === "success"
    ? interpolateHue(bounded, [[0, 6], [50, 16], [70, 34], [85, 68], [95, 126], [100, 158]])
    : kind === "intercept"
      ? interpolateHue(bounded, [[0, 160], [5, 85], [20, 40], [50, 16], [100, 2]])
      : interpolateHue(bounded, [[0, 8], [20, 24], [40, 42], [60, 76], [80, 132], [100, 166]]);
  const saturation = bounded === undefined ? 0 : kind === "success" ? 47 : kind === "intercept" ? 18 + 30 * Math.min(1, bounded / 25) : 48 + bounded * 0.14;
  const palette = bounded === undefined ? undefined : { "--ring-hue": hue, "--ring-saturation": `${saturation}%` } as CSSProperties;
  return (
    <div className="audit-metric-ring" data-kind={kind} data-tone={bounded === undefined ? "empty" : "value"} data-zero={bounded === 0} style={palette} role="img" aria-label={`${label} ${shown}. ${description}${scope ? ` · ${scope}` : ""}`}>
      <svg viewBox="0 0 80 80" aria-hidden="true">
        <circle cx="40" cy="40" r="35" className="audit-ring-track" />
        <circle cx="40" cy="40" r="35" pathLength="100" strokeDasharray="100" strokeDashoffset={100 - (bounded ?? 0)} className="audit-ring-value" />
        {bounded !== undefined && bounded > 0 && bounded < 100 ? <circle cx="75" cy="40" r="2.75" className="audit-ring-tip" style={{ transform: `rotate(${bounded * 3.6}deg)` }} /> : null}
      </svg>
      <span><strong>{shown}</strong><small>{label}</small></span>
      {scope ? <em className="audit-ring-scope">{scope}</em> : null}
    </div>
  );
}
