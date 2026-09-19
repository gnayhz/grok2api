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
});

test("mixed records share counts and result filters without counting a batch twice", () => {
  const legacy = { ...task(3, []), direction: "exit_jury", proof: undefined, result: "clean" };
  const rows = [task(1, ["degraded", "degraded"]), task(2, ["healthy"]), legacy, task(4, ["healthy"], "running"), task(5, [], "cancelled")];
  deepStrictEqual(probeSummary(rows, now), { pending: 0, running: 1, inFlight: 1, cancelled: 1, clean: 2, degraded: 1, error: 0 });
  const select = (direction: string, result: string) => filterProbeRecords(rows, direction, result, "", new Map(), new Map(), new Map()).map(row => row.id);
  deepStrictEqual(select("all", "clean"), [3, 2]);
  deepStrictEqual(select("case_proof", "clean"), [2]);
  deepStrictEqual(select("all", "degraded"), [1]);
  deepStrictEqual(select("all", "cancelled"), [5]);
});

test("search includes proof-only identities, exact large IDs, control names and recorded exit IP", () => {
  const row = task(1, ["healthy"]);
  row.proof!.results![0].resource_id = "9007199254740993";
  row.control_account_id = 4;
  row.control_node_id = 9;
  row.control_epoch = 1;
  const accounts = new Map([["9007199254740993", { name: "Fictional target", email: "fictional@example.invalid" }], ["4", { name: "Fictional control" }]]);
  const nodes = new Map([["9", { name: "Fictional comparison exit" }]]);
  const ips = new Map([[9, { currentEpoch: 2, current: "192.0.2.2", epochs: new Map([[1, "192.0.2.1"]]) }]]);
  for (const search of ["fictional target", "fictional@example.invalid", "#9007199254740993", "fictional control", "fictional comparison exit", "192.0.2.1", "#77"]) {
    strictEqual(filterProbeRecords([row], "all", "all", search, accounts, nodes, ips).length, 1, search);
  }
  strictEqual(filterProbeRecords([row], "all", "all", "192.0.2.2", accounts, nodes, ips).length, 0);
  strictEqual(probeReferences(row).accounts.includes("9007199254740993"), true);
});
