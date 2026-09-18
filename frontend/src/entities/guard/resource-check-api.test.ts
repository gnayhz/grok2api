import assert from "node:assert/strict";
import test from "node:test";
import { resourceChecksDecoder } from "./resource-check-api.ts";

test("resource reports retain partial and interrupted comparisons without manufacturing health", () => {
  const sample = { sample: "token-short", outcome: "error", failure: "local/verification/path", thinking: false, completed: false, usage_reported: false, input_tokens: 0, path_verified: false, attempt: { id: "", account_id: "0", model: "", path: { status: "" } } };
  const row = { id: "9007199254740993", kind: "node", resource_id: "7", model: "fictional-model", state: "cancelled", created_at: "2025-01-01T00:00:00Z", report: { version: "resource-quality-check-v1", kind: "node", resource_id: "7", outcome: "inconclusive", reason: "insufficient_controls", calls: 1, max_calls: 28, groups: [{ control_account: "1", control_node: "2", account_id: "1", node_id: "7", control: [sample], samples: [], outcome: "inconclusive", reason: "control_unavailable", control_delta: 0, delta: 0 }] } };
  assert.deepEqual(resourceChecksDecoder({ items: [row] }), { items: [row] });
  assert.throws(() => resourceChecksDecoder({ items: [{ ...row, report: { ...row.report, outcome: "healthy_guess" } }] }));
  assert.throws(() => resourceChecksDecoder({ items: [{ ...row, report: { ...row.report, groups: [{ ...row.report.groups[0], control: [{ ...sample, path_verified: "yes" }] }] } }] }));
});
