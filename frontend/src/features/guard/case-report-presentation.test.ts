import { strict as assert } from "node:assert";
import { test } from "node:test";
import type { QualityCase } from "@/entities/guard/quality-api";
import type { ResourceObservation, ResourceProof, ResourceReport } from "@/entities/guard/resource-check-api";
import { caseReportEvidence, caseReportFinding, caseReportReason } from "./case-report-presentation.ts";
const item: QualityCase = { id: 1, status: "dismissed", verdict: "insufficient", opened_at: "2026-01-01T00:00:00Z", parties: [] };
const proof: ResourceProof = { kind: "account", resource_id: "7", outcome: "healthy", reason: "proved_normal", evidence: [1], window: 1, valid_until: "2026-01-01T00:03:00Z" };
const report: ResourceReport = { version: "resource-proof-v2", kind: "account", resource_id: "7", outcome: "healthy", reason: "proved_normal", calls: 1, max_calls: 5, results: [proof] };
test("case summary distinguishes normal, unknown, provisional and manually released findings", () => {
 assert.equal(caseReportFinding(item), "inconclusive");
 assert.equal(caseReportFinding(item, report), "normal");
 assert.equal(caseReportFinding(item, { ...report, results: [proof, { ...proof, kind: "node", outcome: "inconclusive" }] }), "inconclusive");
 assert.equal(caseReportFinding({ ...item, status: "investigating", verdict: "" }, report), "investigating");
 for (const [verdict, label] of [["account_guilty", "account"], ["exit_guilty", "exit"], ["both_guilty", "both"]]) {
  assert.equal(caseReportFinding({ ...item, status: verdict, verdict }, report), label);
 }
 assert.equal(caseReportFinding({ ...item, evidence: { manual_review: { reason: "Fictional review" } } }, report), "reviewed");
});
test("evidence links use the certificate references and window, not a new inferred comparison", () => {
 const observations = [{ id: 1, window: 1 }, { id: 2, window: 2 }] as ResourceObservation[];
 assert.deepEqual(caseReportEvidence({ ...proof, evidence: [2, 1, 9] }, { ...report, observations }), [observations[0]]);
 assert.equal(caseReportReason({ ...proof, outcome: "inconclusive", reason: "resource_changed", rule: "R2" }), "changed");
 assert.equal(caseReportReason({ ...proof, outcome: "inconclusive", reason: "conflicting_samples" }), "conflict");
});
