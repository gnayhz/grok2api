import { deepStrictEqual, strictEqual } from "node:assert";
import { test } from "node:test";
import type { QualityProbeTask } from "@/entities/guard/quality-api";
import type { ResourceProof } from "@/entities/guard/resource-check-api";
import { filterProbeRecords, getProbeFinding, probeReferences, probeResult, probeSummary } from "./quality-view.ts";

const now = Date.parse("2030-01-01T00:10:00Z");
function task(id: number, outcomes: string[], state = "done"): QualityProbeTask {
  return { id, case_id: 77, direction: "case_proof", defendant: 1, juror: 0, node_id: 8, epoch: 0, state, result: "error", detail: "proved_account_bad", created_at: "2030-01-01T00:00:00Z", finished_at: "2030-01-01T00:01:00Z",
    proof: { version: "resource-proof-v2", kind: "account", resource_id: "1", outcome: "inconclusive", reason: "", calls: 2, max_calls: 6, results: outcomes.map((outcome, i): ResourceProof => ({ kind: i ? "node" : "account", resource_id: i ? "8" : "1", outcome, reason: "", evidence: [], window: 1, valid_until: "2030-01-01T00:03:00Z" })) } };
}

test("case records use target proofs, retain partial findings and respect task terminal state", () => {
  strictEqual(probeResult(task(1, ["healthy", "healthy"])), "clean");
  strictEqual(probeResult(task(1, ["healthy", "inconclusive"])), "error");
  strictEqual(probeResult(task(1, ["degraded", "inconclusive"])), "degraded");
  strictEqual(probeResult(task(1, ["degraded", "degraded"])), "degraded");
  strictEqual(probeResult(task(1, [])), "error");
  for (const state of ["running", "pending", "failed", "cancelled"]) {
    const row = task(1, ["degraded"], state);
    strictEqual(probeResult(row), state === "failed" ? "error" : state === "pending" ? "running" : state);
    strictEqual(getProbeFinding(row, key => key).tone === "bad", false);
  }
  strictEqual(getProbeFinding(task(1, ["healthy"]), key => key).badge, "guardProbes.proofNormalBadge");
  strictEqual(getProbeFinding(task(1, ["degraded", "healthy"]), key => key).badge, "guardProbes.accountAbnormalBadge");
  strictEqual(getProbeFinding(task(1, ["healthy", "degraded"]), key => key).badge, "guardProbes.exitAbnormalBadge");
  strictEqual(getProbeFinding(task(1, ["degraded", "degraded"]), key => key).badge, "guardProbes.bothAbnormalBadge");
  const missing = { ...task(1, []), proof: undefined, result: "clean" };
  strictEqual(probeResult(missing), "error", "generic sample result cannot replace a resource proof");
});

test("proof records share counts and result filters without counting a batch twice", () => {
  const rows = [task(1, ["degraded", "degraded"]), task(2, ["healthy"]), task(3, ["healthy"]), task(4, ["healthy"], "running"), task(5, [], "cancelled")];
  deepStrictEqual(probeSummary(rows, now), { pending: 0, running: 1, inFlight: 1, cancelled: 1, clean: 2, degraded: 1, error: 0 });
  const select = (result: string) => filterProbeRecords(rows, result, "", new Map(), new Map(), new Map()).map(row => row.id);
  deepStrictEqual(select("clean"), [3, 2]);
  deepStrictEqual(select("degraded"), [1]);
  deepStrictEqual(select("cancelled"), [5]);
});

test("search includes proof-only identities, exact large IDs, control names and recorded exit IP", () => {
  const row = task(1, ["healthy"]);
  row.proof!.results![0].resource_id = "9007199254740993";
  row.epoch = 1;
  row.proof!.observations = [{ id: 1, window: 1, account_id: "4", node_id: "9", purpose: "resolve_negative", class: "A",
    sample: { sample: "token-short", outcome: "clean", thinking: true, completed: true, usage_reported: true, input_tokens: 100, path_verified: true,
      attempt: { id: "fictional-control", account_id: "4", model: "fictional-model", path: { node_id: "9", status: "registered" } } } }];
  const accounts = new Map([["9007199254740993", { name: "Fictional target", email: "fictional@example.invalid" }], ["4", { name: "Fictional control" }]]);
  const nodes = new Map([["9", { name: "Fictional comparison exit" }]]);
  const ips = new Map([[8, { currentEpoch: 2, current: "192.0.2.2", epochs: new Map([[1, "192.0.2.1"]]) }]]);
  for (const search of ["fictional target", "fictional@example.invalid", "#9007199254740993", "fictional control", "fictional comparison exit", "192.0.2.1", "#77"]) {
    strictEqual(filterProbeRecords([row], "all", search, accounts, nodes, ips).length, 1, search);
  }
  strictEqual(filterProbeRecords([row], "all", "192.0.2.2", accounts, nodes, ips).length, 0);
  strictEqual(probeReferences(row).accounts.includes("9007199254740993"), true);
});
