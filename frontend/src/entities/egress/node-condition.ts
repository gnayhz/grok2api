import type { EgressNodeDTO } from "@/entities/egress/egress-api";

export type NodeCondition =
	| "ready"
	| "dynamic"
	| "held"
	| "banned"
	| "cooling"
	| "unhealthy"
	| "unknown"
	| "disabled"
	| "unconfigured";
/** Reachability and quality are independent axes; a 100% health score must
 * never present an isolated exit as ready. Dynamic tunnels are labelled explicitly. */
export function nodeCondition(node: EgressNodeDTO, now: number): NodeCondition {
	if (node.quality?.state === "banned") return "banned";
	if (node.quality?.state === "remanded") return "held";
	if (!node.enabled) return "disabled";
	if (!node.proxyConfigured) return "unconfigured";
	if (node.cooldownUntil && Date.parse(node.cooldownUntil) > now)
		return "cooling";
	if (node.rotatingEndpoint) return "dynamic";
	if (node.probeStatus === "unhealthy") return "unhealthy";
	if (node.probeStatus !== "healthy") return "unknown";
	return "ready";
}
function nodeNeedsAttention(node: EgressNodeDTO, now: number): boolean {
	return ["held", "banned", "cooling", "unhealthy", "unconfigured"].includes(
		nodeCondition(node, now),
	);
}
export function networkSummary(nodes: EgressNodeDTO[], now: number) {
	const conditions = nodes.map((n) => nodeCondition(n, now));
	const latencies = nodes
		.filter(
			(n) =>
				n.enabled &&
				!n.rotatingEndpoint &&
				n.probeStatus === "healthy" &&
				n.probeLatencyMs > 0,
		)
		.map((n) => n.probeLatencyMs)
		.sort((a, b) => a - b);
	const mid = Math.floor(latencies.length / 2);
	return {
		total: nodes.length,
		ready: conditions.filter((c) => c === "ready" || c === "dynamic").length,
		attention: nodes.filter((n) => nodeNeedsAttention(n, now)).length,
		unknown: conditions.filter((c) => c === "unknown").length,
		disabled: conditions.filter((c) => c === "disabled").length,
		dynamic: conditions.filter((c) => c === "dynamic").length,
		median: latencies.length
			? Math.round(
					latencies.length % 2
						? latencies[mid]!
						: (latencies[mid - 1]! + latencies[mid]!) / 2,
				)
			: null,
	};
}
