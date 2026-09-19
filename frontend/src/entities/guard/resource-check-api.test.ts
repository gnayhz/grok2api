import assert from "node:assert/strict";
import test from "node:test";
import { resourceChecksDecoder } from "./resource-check-api.ts";

test("proof batches decode a durable reservation and shared per-resource certificates", () => {
  const sample = { sample: "", outcome: "", thinking: false, completed: false, usage_reported: false, input_tokens: 0, path_verified: false, attempt: { id: "", account_id: "0", model: "", path: { status: "" } } };
  const report = { version: "resource-proof-v2", kind: "account", resource_id: "1", outcome: "inconclusive", reason: "insufficient_controls", calls: 1, max_calls: 6, generations: 0, path_checks: 0,
    observations: [{ id: 1, window: 1, account_id: "1", node_id: "2", purpose: "measure_target", class: "pending", sample }],
    results: [{ kind: "account", resource_id: "1", outcome: "inconclusive", reason: "insufficient_controls", evidence: [], window: 1, valid_until: "2025-01-01T00:03:00Z" }] };
  const row = { id: "41", kind: "account", resource_id: "1", model: "fictional-model", state: "running", created_at: "2025-01-01T00:00:00Z", report };
  assert.deepEqual(resourceChecksDecoder({items: [row]}), {items: [row]});
  assert.throws(() => resourceChecksDecoder({items: [{...row, report: {...report, version: "retired-protocol"}}]}));
  assert.throws(() => resourceChecksDecoder({items: [{...row, report: {...report, observations: [{...report.observations[0], sample: {...sample, path_verified: "yes"}}]}}]}));
  const completed = { ...row, state: "done", report: { ...report, outcome: "degraded", reason: "proved_account_bad", results: [{ ...report.results[0], outcome: "degraded", rule: "R2", evidence: [1, 2] }] } };
  assert.deepEqual(resourceChecksDecoder({items: [completed]}), {items: [completed]});
  assert.throws(() => resourceChecksDecoder({items: [{ ...row, report: { ...report, results: [{ ...report.results[0], evidence: ["1"] }] } }]}));
});
