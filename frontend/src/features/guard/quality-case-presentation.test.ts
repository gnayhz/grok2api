import { strict as assert } from "node:assert";
import { test } from "node:test";
import type { QualityCase } from "@/entities/guard/quality-api";
import {
	caseDispositionKey,
 partyDispositionKey,
	caseEarlyRelease,
} from "./quality-case-presentation.ts";
function example(account: string, exit: string): QualityCase {
	return {
		id: 1,
		status: "investigating",
		verdict: "",
		opened_at: "2026-09-06T00:00:00Z",
		parties: [
			{
				kind: "account",
				account_id: 7,
				node_id: 0,
				epoch: 0,
				role: "defendant",
				disposition: account,
			},
			{
				kind: "exit",
				account_id: 0,
				node_id: 3,
				epoch: 0,
				role: "co_remanded",
				disposition: exit,
			},
		],
	};
}
test("an open case describes each actual hold independently", () => {
	assert.equal(
		caseDispositionKey(example("remanded", "released")),
		"exitClearedAccountPending",
	);
	assert.equal(
		caseDispositionKey(example("released", "remanded")),
		"accountClearedExitPending",
	);
	assert.equal(caseDispositionKey(example("remanded", "remanded")), "bothHeld");
	assert.equal(
		caseDispositionKey(example("released", "released")),
		"bothReleased",
	);
	assert.equal(
		caseDispositionKey(example("remanded", "withdrawn")),
		"exitClearedAccountPending",
	);
});
test("early release explanation comes from committed evidence, never inferred from a clean count", () => {
	const item = example("remanded", "released");
	assert.equal(caseEarlyRelease(item, "exit"), undefined);
	item.evidence = {
		early_releases: {
			exit: { at: "2026-09-06T00:01:00Z", reason: "exit_controls_clean" },
		},
	};
	assert.deepEqual(caseEarlyRelease(item, "exit"), {
		at: "2026-09-06T00:01:00Z",
		reason: "exit_controls_clean",
	});
	assert.equal(caseEarlyRelease(item, "account"), undefined);
});

test("sentenced and released parties are never presented as healthy", () => {
 assert.equal(partyDispositionKey("sentenced"), "experiment.dispositions.sentenced");
 assert.equal(partyDispositionKey("released"), "experiment.dispositions.released");
 assert.equal(partyDispositionKey("unexpected"), "experiment.dispositions.unknown");
});

test("a dual verdict displays restrictions and preserves partial unknown releases", () => {
 const both = example("sentenced", "sentenced");
 both.status = both.verdict = "both_guilty";
 assert.equal(caseDispositionKey(both), "bothRestricted");
 const partial = example("sentenced", "released");
 partial.status = partial.verdict = "account_guilty";
 assert.equal(caseDispositionKey(partial), "accountRestricted");
 assert.equal(partyDispositionKey(partial.parties[1].disposition), "experiment.dispositions.released");
});
