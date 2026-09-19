import type { ResourceCheck, ResourceKind, ResourceObservation, ResourceProof } from "./resource-check-api";

export type ResourceFinding = "healthy" | "degraded" | "inconclusive" | "pending" | "running" | "failed" | "cancelled" | "empty";

export function resourceHistory(checks: ResourceCheck[], kind: ResourceKind, id: string) {
  return checks.filter(check => check.kind === kind && check.resource_id === id).sort((a, b) =>
    Date.parse(b.created_at) - Date.parse(a.created_at) || b.id.length - a.id.length || b.id.localeCompare(a.id));
}

export function resourceProof(check: ResourceCheck): ResourceProof | undefined {
  return check.report?.results?.find(proof => proof.kind === check.kind && proof.resource_id === check.resource_id);
}

// A shared batch may contain other resources' certificates and unfinished findings.
// Display the selected target's recorded result only after the task has completed.
export function resourceFinding(check?: ResourceCheck): ResourceFinding {
  if (!check) return "empty";
  if (check.state !== "done") return check.state;
  const outcome = resourceProof(check)?.outcome ?? check.report?.outcome;
  return outcome === "healthy" || outcome === "degraded" ? outcome : "inconclusive";
}

export function resourceEvidence(check: ResourceCheck): ResourceObservation[] {
  const proof = resourceProof(check);
  return (check.report?.observations ?? []).filter(o => proof?.window === o.window && proof.evidence.includes(o.id));
}

export function resourceObservations(check: ResourceCheck): ResourceObservation[] {
  const evidence = new Set(resourceEvidence(check));
  return (check.report?.observations ?? []).filter(o => evidence.has(o) ||
    (check.kind === "account" ? o.account_id : o.node_id) === check.resource_id);
}

export function resourceNormalExpired(check: ResourceCheck | undefined, now: number) {
  if (!check || resourceFinding(check) !== "healthy") return false;
  const expires = resourceProof(check)?.valid_until;
  return !!expires && Date.parse(expires) <= now;
}
