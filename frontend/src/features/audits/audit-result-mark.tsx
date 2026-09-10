import { Ban, Check, CircleHelp, Clock3, FileX2, Gauge, KeyRound, LockKeyhole, ServerOff, Shield, ShieldAlert, ShieldCheck, ShieldOff, ShieldX, Unplug, Wallet, Wrench, X, ZapOff } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { auditGuardState, auditOutcome, type AuditGuardState, type AuditOutcome } from "./audit-presentation";
import type { AuditDTO } from "./request-audits-api";

const outcomeIcons = {
  firstSuccess: Check, retrySuccess: Check, qualityReleased: Check, qualityBlocked: ShieldX,
  rateLimited: Gauge, quotaExceeded: Wallet, unauthorized: KeyRound, forbidden: LockKeyhole,
  timeout: Clock3, canceled: Ban, streamInterrupted: ZapOff, networkError: Unplug,
  unavailable: ServerOff, invalidRequest: FileX2, processingFailed: Wrench, failed: X, unknown: CircleHelp,
} satisfies Record<AuditOutcome, typeof Check>;

const guardIcons = { passed: ShieldCheck, intervened: Shield, released: ShieldAlert, blocked: ShieldX, bypassed: ShieldOff } satisfies Record<Exclude<AuditGuardState, "unrecorded">, typeof Shield>;

function ResultMark({ outcome, count = 0, guard = "unrecorded" }: { outcome: AuditOutcome; count?: number; guard?: AuditGuardState }) {
  const Icon = outcomeIcons[outcome];
  const Guard = guard === "unrecorded" ? undefined : guardIcons[guard];
  return <span className="audit-result-mark" data-outcome={outcome} data-recovered={count > 0 && ["retrySuccess", "qualityReleased"].includes(outcome)} aria-hidden="true">
    <span className="audit-result-disc"><Icon className="audit-result-symbol" /></span>
    {count > 0 ? <>
      <svg className="audit-result-orbit" viewBox="0 0 44 44" fill="none"><circle cx="22" cy="22" r="19" /></svg>
      <span className="audit-result-count">{count > 99 ? "99+" : count}</span>
    </> : null}
    {Guard ? <span className="audit-result-guard" data-guard={guard}><Shield className="audit-result-guard-ground" /><Guard className="audit-result-guard-symbol" /></span> : null}
  </span>;
}

export function AuditResultButton({ audit, onOpen }: { audit: AuditDTO; onOpen: () => void }) {
  const { t } = useTranslation();
  const outcome = auditOutcome(audit);
  const guard = auditGuardState(audit);
  const label = [t("audits.outcomes." + outcome), audit.attemptCount > 0 ? t("audits.recordedFailures", { count: audit.attemptCount }) : "", t("audits.guardMarks." + guard), t("audits.openRequest", { id: audit.requestId })].filter(Boolean).join(" · ");
  return <button type="button" className="audit-result-button" aria-label={label} onClick={onOpen}>
    <ResultMark outcome={outcome} count={audit.attemptCount} guard={guard} />
  </button>;
}

const legendOutcomes: AuditOutcome[] = ["rateLimited", "quotaExceeded", "unauthorized", "forbidden", "timeout", "canceled", "streamInterrupted", "networkError", "unavailable", "invalidRequest", "processingFailed", "failed", "qualityBlocked", "unknown"];

export function AuditResultLegend() {
  const { t } = useTranslation();
  return <Popover>
    <PopoverTrigger asChild><Button variant="ghost" size="sm" className="h-8 gap-1.5 text-xs text-muted-foreground"><CircleHelp className="size-3.5" />{t("audits.resultLegend")}</Button></PopoverTrigger>
    <PopoverContent align="end" className="audit-result-legend w-[370px] max-w-[calc(100vw-24px)] max-h-[min(680px,80dvh)] overflow-y-auto p-4" aria-label={t("audits.resultLegend")}>
      <p className="text-xs font-medium">{t("audits.resultLegend")}</p>
      <div className="audit-legend-success">
        {[0, 1, 3].map((count) => <div key={count}><ResultMark outcome={count ? "retrySuccess" : "firstSuccess"} count={count} /><span>{count ? t("audits.recoveredMark", { count }) : t("audits.outcomes.firstSuccess")}</span></div>)}
      </div>
      <p className="text-[11px] leading-5 text-muted-foreground">{t("audits.attemptMarkHelp")}</p>
      <div className="audit-legend-failures">{legendOutcomes.map((outcome) => {
        const Icon = outcomeIcons[outcome];
        return <div key={outcome} data-outcome={outcome}><Icon aria-hidden="true" /><span>{t("audits.outcomes." + outcome)}</span></div>;
      })}</div>
      <div className="audit-legend-guards">{(Object.keys(guardIcons) as Array<keyof typeof guardIcons>).map((guard) => {
        const Icon = guardIcons[guard];
        return <div key={guard}><span className="audit-legend-shield" data-guard={guard}><Icon aria-hidden="true" /></span><span>{t("audits.guardMarks." + guard)}</span></div>;
      })}</div>
      <p className="mt-3 text-[11px] leading-5 text-muted-foreground">{t("audits.guardMarkHelp")}</p>
    </PopoverContent>
  </Popover>;
}
