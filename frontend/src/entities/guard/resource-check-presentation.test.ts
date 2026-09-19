import { strict as assert } from "node:assert";
import { test } from "node:test";
import type { ResourceCheck, ResourceObservation, ResourceProof } from "./resource-check-api";
import { resourceEvidence, resourceFinding, resourceHistory, resourceNormalExpired, resourceObservations } from "./resource-check-presentation.ts";

const proof: ResourceProof = { kind: "account", resource_id: "9007199254740993", outcome: "healthy", reason: "proved_normal", evidence: [2], window: 2, valid_until: "2026-01-01T00:03:00Z" };
const check: ResourceCheck = { id: "9007199254740993", kind: "account", resource_id: proof.resource_id, model: "fictional-model", state: "done", created_at: "2026-01-01T00:00:00Z", report: { version: "resource-proof-v2", kind: "account", resource_id: proof.resource_id, outcome: "degraded", reason: "proved_account_bad", calls: 3, max_calls: 6, results: [{ ...proof, resource_id: "9007199254740992", outcome: "degraded" }, proof] } };

test("batch findings select the exact target and keep provisional, failed and cancelled states", () => {
  assert.equal(resourceFinding(check), "healthy");
  for (const state of ["pending", "running", "failed", "cancelled"] as const) assert.equal(resourceFinding({ ...check, state }), state);
  assert.equal(resourceFinding(), "empty");
  assert.equal(resourceFinding({ ...check, report: undefined }), "inconclusive");
  assert.equal(resourceFinding({ ...check, report: { ...check.report!, results: undefined } }), "degraded");
  assert.equal(resourceFinding({ ...check, report: { ...check.report!, results: [{ ...proof, outcome: "future_unknown" }] } }), "inconclusive");
});

test("history sorts without rounding decimal string IDs or mixing kinds and targets", () => {
  const old = { ...check, id: "9007199254740992" };
  const fresh = { ...check, id: "9007199254740994", created_at: "2026-01-02T00:00:00Z" };
  const items = [old, { ...check, resource_id: "9007199254740992" }, { ...check, kind: "node" as const }, check, fresh];
  assert.deepEqual(resourceHistory(items, "account", check.resource_id), [fresh, check, old]);
  assert.equal(items[0], old);
});

test("related checks include the certificate's controls but exclude other targets and other windows' evidence", () => {
  const observations = [{ id: 1, window: 2, account_id: check.resource_id, node_id: "7" }, { id: 2, window: 1, account_id: "2", node_id: "8" }, { id: 2, window: 2, account_id: "2", node_id: "7" }, { id: 3, window: 2, account_id: "3", node_id: "8" }] as ResourceObservation[];
  const batch = { ...check, report: { ...check.report!, observations } };
  assert.deepEqual(resourceEvidence(batch), [observations[2]]);
  assert.deepEqual(resourceObservations(batch), [observations[0], observations[2]]);
  assert.deepEqual(resourceObservations({ ...batch, kind: "node", resource_id: "8" }), [observations[1], observations[3]]);
});

test("normal expiry describes past evidence without replacing the recorded finding", () => {
  assert.equal(resourceNormalExpired(check, Date.parse(proof.valid_until) - 1), false);
  assert.equal(resourceNormalExpired(check, Date.parse(proof.valid_until)), true);
  assert.equal(resourceFinding(check), "healthy");
  assert.equal(resourceNormalExpired({ ...check, state: "running" }, Date.parse(proof.valid_until)), false);
  assert.equal(resourceNormalExpired({ ...check, report: { ...check.report!, results: [{ ...proof, outcome: "degraded" }] } }, Date.parse(proof.valid_until)), false);
});
