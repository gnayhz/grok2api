import type { QualityCase } from "@/entities/guard/quality-api";

/** Case progress and resource eligibility are independent. Never infer both
 * holds from an open case, nor global eligibility from this case's release. */
export function caseDispositionKey(item: QualityCase): string {
	const held = (kind: string) =>
		item.parties.some(
			(p) =>
				p.kind === kind && ["remanded", "sentenced"].includes(p.disposition),
		);
	const account = held("account"),
		exit = held("exit");
	if (account && exit) return item.status === "investigating" ? "bothHeld" : "bothRestricted";
	if (account)
		return item.status === "investigating"
			? "exitClearedAccountPending"
			: "accountRestricted";
	if (exit)
		return item.status === "investigating"
			? "accountClearedExitPending"
			: "exitRestricted";
	return "bothReleased";
}
export function caseEarlyRelease(
	item: QualityCase,
	kind: string,
): { at?: string; reason?: string } | undefined {
	const values = item.evidence?.early_releases;
	if (!values || typeof values !== "object") return undefined;
	const entry = (values as Record<string, unknown>)[kind];
	if (!entry || typeof entry !== "object") return undefined;
	const { at, reason } = entry as Record<string, unknown>;
	return {
		at: typeof at === "string" ? at : undefined,
		reason: typeof reason === "string" ? reason : undefined,
	};
}

/** Resource disposition describes this case's hold, not account health. */
export function partyDispositionKey(disposition: string): string {
	return ["remanded", "sentenced", "released", "withdrawn", "dismissed"].includes(disposition)
		? `experiment.dispositions.${disposition}` : "experiment.dispositions.unknown";
}
