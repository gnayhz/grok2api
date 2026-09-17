import { deepStrictEqual, strictEqual } from "node:assert";
import { test } from "node:test";
import { networkSummary, nodeCondition } from "./node-condition.ts";
import type { EgressNodeDTO } from "@/entities/egress/egress-api";
const now = Date.parse("2026-09-06T00:00:00Z");
function node(overrides: Partial<EgressNodeDTO> = {}): EgressNodeDTO {
	return {
		id: "1",
		name: "Exit",
		enabled: true,
		proxyConfigured: true,
		rotatingEndpoint: false,
		probeStatus: "healthy",
		probeLatencyMs: 100,
		health: 1,
		...overrides,
	} as EgressNodeDTO;
}
test("quality isolation takes precedence over a perfect transport health score", () => {
	strictEqual(
		nodeCondition(node({ quality: { state: "banned" } }), now),
		"banned",
	);
	strictEqual(
		nodeCondition(node({ quality: { state: "remanded" } }), now),
		"held",
	);
	strictEqual(
		networkSummary([node({ quality: { state: "banned" } })], now).ready,
		0,
	);
});
test("untested, disabled and cooling exits are not shown as eligible", () => {
	strictEqual(nodeCondition(node({ probeStatus: "unknown" }), now), "unknown");
	strictEqual(nodeCondition(node({ enabled: false }), now), "disabled");
	strictEqual(
		nodeCondition(node({ cooldownUntil: "2026-09-06T00:01:00Z" }), now),
		"cooling",
	);
	strictEqual(
		nodeCondition(node({ cooldownUntil: "2026-09-05T23:59:00Z" }), now),
		"ready",
	);
});
test("dynamic tunnels are explicit, and latency medians use measured fixed exits", () => {
	const summary = networkSummary(
		[
			node({ probeLatencyMs: 100 }),
			node({ probeLatencyMs: 300 }),
			node({ rotatingEndpoint: true, probeLatencyMs: 9000 }),
			node({ enabled: false, probeLatencyMs: 9000 }),
			node({ probeStatus: "unknown", probeLatencyMs: 0 }),
		],
		now,
	);
	deepStrictEqual(summary, {
		total: 5,
		ready: 3,
		attention: 0,
		unknown: 1,
		disabled: 1,
		dynamic: 1,
		median: 200,
	});
	strictEqual(networkSummary([], now).median, null);
});
