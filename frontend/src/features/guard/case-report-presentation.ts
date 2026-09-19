import type { QualityCase } from "@/entities/guard/quality-api";
import type { ResourceObservation, ResourceProof, ResourceReport } from "@/entities/guard/resource-check-api";

/** Describe the recorded finding, without deriving a verdict from probe counts. */
export function caseReportFinding(item: QualityCase, report?: ResourceReport): string {
 if (item.evidence?.manual_review) return "reviewed";
 if (item.status === "investigating") return "investigating";
 if (item.verdict === "both_guilty") return "both";
 if (item.verdict === "account_guilty") return "account";
 if (item.verdict === "exit_guilty") return "exit";
 const targets = report?.results;
 return targets?.length && targets.every(p => p.outcome === "healthy") ? "normal" : "inconclusive";
}

/** Only cite observations actually referenced by the recorded certificate. */
export function caseReportEvidence(proof: ResourceProof, report: ResourceReport): ResourceObservation[] {
 return proof.evidence.flatMap(id => {
  const observation = report.observations?.find(o => o.id === id && o.window === proof.window);
  return observation ? [observation] : [];
 });
}

export function caseReportReason(proof: ResourceProof): string {
 if (proof.outcome === "healthy") return "normal";
 if (proof.outcome === "degraded") return proof.kind === "account" ? "account" : "exit";
 if (proof.reason === "conflicting_samples") return "conflict";
 if (["window_expired", "resource_changed", "identity_changed", "path_changed", "baseline_epoch_changed"].includes(proof.reason)) return "changed";
 return "inconclusive";
}
