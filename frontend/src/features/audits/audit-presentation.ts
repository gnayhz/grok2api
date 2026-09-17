import type { AuditBillingBreakdownDTO, AuditDTO } from "@/entities/audit/audit-api";

export function auditProviderLabel(provider: AuditDTO["provider"]): string {
  return { grok_build: "Build", grok_web: "Web", grok_console: "Console" }[provider];
}

export type AuditResult = "success" | "failed" | "unknown";

// HTTP headers can have been sent before a stream failed. The error remains
// visible beside the original status; a 200 alone is not a successful request.
export function auditResult(audit: Pick<AuditDTO, "statusCode" | "errorCode">): AuditResult {
  if (audit.errorCode || audit.statusCode >= 400) return "failed";
  if (audit.statusCode >= 200 && audit.statusCode < 300) return "success";
  return "unknown";
}

export function auditBilling(audit: Pick<AuditDTO, "billing" | "costInUsdTicks" | "pricingModel" | "pricingVersion" | "estimatedCostInUsdTicks">): AuditBillingBreakdownDTO | undefined {
  if (audit.billing) return audit.billing;
  if (audit.costInUsdTicks > 0) {
    return { source: "upstream", method: "upstream_reported", components: [], totalInUsdTicks: audit.costInUsdTicks };
  }
  if (!audit.pricingModel) return undefined;
  return {
    source: "official",
    method: "stored_estimate",
    model: audit.pricingModel,
    version: audit.pricingVersion,
    components: [],
    totalInUsdTicks: audit.estimatedCostInUsdTicks,
  };
}

export function completionTone(outcome?: string): "success" | "failed" | "warning" | "neutral" {
  if (["admitted", "completed", "committed"].includes(outcome ?? "")) return "success";
  if (["failed", "not_admitted"].includes(outcome ?? "")) return "failed";
  if (["canceled", "unconfirmed", "not_committed", "partial"].includes(outcome ?? "")) return "warning";
  return "neutral";
}

export type AuditOutcome = "firstSuccess" | "retrySuccess" | "qualityReleased" | "qualityBlocked" | "rateLimited" | "quotaExceeded" | "unauthorized" | "forbidden" | "timeout" | "canceled" | "streamInterrupted" | "networkError" | "unavailable" | "invalidRequest" | "processingFailed" | "failed" | "unknown";

export type AuditGuardState = "passed" | "intervened" | "released" | "blocked" | "bypassed" | "unrecorded";

export function auditGuardState(audit: Pick<AuditDTO, "qualityRule" | "qualityExempt" | "qualityFailOpen" | "errorCode">): AuditGuardState {
  if (audit.errorCode === "quality_degraded" || audit.errorCode?.startsWith("quality_degraded_")) return "blocked";
  if (audit.qualityFailOpen) return "released";
  if (audit.qualityExempt) return "bypassed";
  if (audit.qualityRule === "thinking") return "passed";
  if (audit.qualityRule || audit.errorCode?.startsWith("quality_")) return "intervened";
  return "unrecorded";
}

// Read the recorded gateway result; upstream errors may append a normalized
// provider suffix. Status alone cannot distinguish a failed 200 stream.
export function auditOutcome(audit: Pick<AuditDTO, "statusCode" | "errorCode" | "attemptCount" | "qualityFailOpen">): AuditOutcome {
  const result = auditResult(audit);
  if (result === "success") return audit.qualityFailOpen ? "qualityReleased" : audit.attemptCount > 0 ? "retrySuccess" : "firstSuccess";
  if (result === "unknown") return "unknown";
  const code = audit.errorCode ?? "";
  const matches = (...values: string[]) => values.some((value) => code === value || code.startsWith(value + "_"));
  if (matches("request_canceled", "client_disconnected") || audit.statusCode === 499) return "canceled";
  if (matches("quality_degraded")) return "qualityBlocked";
  if (matches("upstream_quota_exhausted", "upstream_payment_required", "budget_exceeded", "spending_limit_exceeded") || audit.statusCode === 402) return "quotaExceeded";
  if (matches("upstream_rate_limited", "rate_limited", "client_key_rate_limited") || audit.statusCode === 429) return "rateLimited";
  if (matches("upstream_unauthorized", "upstream_credential_unavailable", "invalid_api_key", "credential_refresh_failed") || audit.statusCode === 401) return "unauthorized";
  if (matches("upstream_forbidden", "model_not_allowed") || audit.statusCode === 403) return "forbidden";
  if (matches("upstream_timeout", "upstream_header_timeout", "upstream_first_token_timeout", "upstream_stream_timeout", "upstream_stream_idle_timeout", "quality_evidence_timeout", "quality_created_timeout", "request_timeout") || [408, 504].includes(audit.statusCode)) return "timeout";
  if (matches("upstream_stream_interrupted", "upstream_stream_empty", "upstream_stream_incomplete", "upstream_stream_error")) return "streamInterrupted";
  if (matches("upstream_network_error", "egress_connection_failed")) return "networkError";
  if (matches("unsafe_replay_blocked", "response_conversion_failed", "media_post_processing_failed", "history_store_unavailable", "history_commit_failed")) return "processingFailed";
  if (matches("upstream_unavailable", "upstream_cooling", "upstream_saturated", "upstream_model_unavailable", "upstream_server_error", "response_resource_exhausted", "quality_guard_unavailable", "physical_attempt_limit") || audit.statusCode === 503) return "unavailable";
  if ([400, 404, 405, 413, 422].includes(audit.statusCode)) return "invalidRequest";
  return "failed";
}
