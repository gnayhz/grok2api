import type { QualityNodeView, QualityProbeTask } from "@/entities/guard/quality-api";
// 仲裁/调查页面使用的最小身份投影。账号与节点的完整管理 DTO 不应
// 进入质量视图模型，质量页面只需要可读名称、邮箱和稳定编号。
export type QualityAccountIdentity = {
	name?: string;
	email?: string;
};

export type QualityNodeIdentity = {
	name?: string;
};

export type QualityExitIPIndex = Map<number, {
	current: string;
	currentEpoch: number;
	epochs: Map<number, string>;
}>;

type QualityIdentityDisplay = {
	primary: string;
	secondary: string;
	title: string;
};

function cleanText(value: string | undefined): string {
	return value?.trim() ?? "";
}

function uniqueText(values: string[]): string[] {
	const seen = new Set<string>();
	return values.filter((value) => {
		const normalized = value.toLocaleLowerCase();
		if (!value || seen.has(normalized)) {
			return false;
		}
		seen.add(normalized);
		return true;
	});
}

/** 账号展示名优先使用人工配置名称，同时保留邮箱和内部 ID 作为核对信息。 */
export function qualityAccountDisplay(id: number, identity?: QualityAccountIdentity): QualityIdentityDisplay {
	const name = cleanText(identity?.name);
	const email = cleanText(identity?.email);
	const idLabel = `#${id}`;
	const primary = name || email || idLabel;
	const secondary = uniqueText([email !== primary ? email : "", primary !== idLabel ? idLabel : ""]).join(" · ");
	return {
		primary,
		secondary,
		title: uniqueText([name, email, idLabel]).join(" · "),
	};
}

/** 出口展示名优先使用节点名称，并同时显示 node+epoch；IP 仅作为辅助核对信息。 */
export function qualityExitDisplay(
	node: number,
	epoch: number,
	identity?: QualityNodeIdentity,
	ipByNode?: QualityExitIPIndex,
): QualityIdentityDisplay {
	const name = cleanText(identity?.name);
	const nodeLabel = `#${node}`;
	const epochLabel = `${nodeLabel}@${epoch}`;
	const nodeInfo = ipByNode?.get(node);
	const ip = nodeInfo?.epochs.get(epoch) ?? (nodeInfo?.currentEpoch === epoch ? nodeInfo.current : "");
	const primary = name || nodeLabel;
	const secondary = uniqueText([epochLabel, ip ?? ""]).join(" · ");
	return {
		primary,
		secondary,
		title: uniqueText([name, epochLabel, ip]).join(" · "),
	};
}

/** 将质量出口档案构造成按 node 查找的 epoch/IP 索引，避免各组件各自猜当前 IP。 */
export function buildQualityExitIPIndex(nodes: QualityNodeView[]): QualityExitIPIndex {
	const result: QualityExitIPIndex = new Map();
	for (const node of nodes) {
		const epochs = new Map<number, string>();
		for (const entry of node.degrade_detail) {
			if (entry.ip && !epochs.has(entry.epoch)) {
				epochs.set(entry.epoch, entry.ip);
			}
		}
		result.set(node.node_id, { current: node.current_ip, currentEpoch: node.current_epoch, epochs });
	}
	return result;
}

export function parseGoDurationMs(value: string | null | undefined): number | null {
	if (!value) {
		return null;
	}
	const scale: Record<string, number> = { ns: 1e-6, us: 1e-3, "µs": 1e-3, ms: 1, s: 1e3, m: 6e4, h: 3.6e6 };
	const segment = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/y;
	const text = value.trim();
	let pos = 0;
	let total = 0;
	while (pos < text.length) {
		segment.lastIndex = pos;
		const match = segment.exec(text);
		if (!match || match.index !== pos) {
			return null;
		}
		const unitScale = scale[match[2]!];
		if (unitScale === undefined) {
			return null;
		}
		total += Number(match[1]) * unitScale;
		pos += match[0].length;
	}
	return pos === 0 ? null : total;
}


const PROBE_RECENT_WINDOW_MS = 30 * 60 * 1000;

// Task completion, sample findings and resource proofs are separate contracts.
// A case proof's generic result is not its per-resource conclusion.
export function probeResult(task: QualityProbeTask): "clean" | "degraded" | "error" | "running" | "cancelled" {
	if (task.state === "pending" || task.state === "running") return "running";
	if (task.state === "cancelled") return "cancelled";
	if (task.state === "failed") return "error";
	if (task.state !== "done") return "error";
	const results = task.proof?.results ?? [];
	if (results.some(result => result.outcome === "degraded")) return "degraded";
	return results.length > 0 && results.every(result => result.outcome === "healthy") ? "clean" : "error";
}

export function probeReferences(task: QualityProbeTask): { accounts: string[]; nodes: string[] } {
	const ids = (values: (string | number | undefined)[]) => [...new Set(values.filter(value => value && String(value) !== "0").map(String))];
	return {
		accounts: ids([task.defendant,
			...(task.proof?.results?.filter(p => p.kind === "account").map(p => p.resource_id) ?? []),
			...(task.proof?.observations?.map(o => o.account_id) ?? [])]),
		nodes: ids([task.node_id,
			...(task.proof?.results?.filter(p => p.kind === "node").map(p => p.resource_id) ?? []),
			...(task.proof?.observations?.map(o => o.node_id) ?? [])]),
	};
}

export function filterProbeRecords(tasks: QualityProbeTask[], result: string, search: string,
	accounts: Map<string, QualityAccountIdentity>, nodes: Map<string, QualityNodeIdentity>, ips: QualityExitIPIndex) {
	const query = search.trim().toLowerCase();
	return tasks.filter(task => {
		if (result !== "all" && probeResult(task) !== result) return false;
		if (!query) return true;
		const refs = probeReferences(task);
		const fields = [String(task.id), `#${task.id}`, String(task.case_id), `#${task.case_id}`, task.experiment?.baseline.model,
			...refs.accounts.flatMap(id => [id, `#${id}`, accounts.get(id)?.name, accounts.get(id)?.email]),
			...refs.nodes.flatMap(id => [id, `#${id}`, nodes.get(id)?.name])];
		// Only the recorded epoch may supply an IP, never a node's newer address.
		if (task.node_id && task.epoch !== undefined) {
			const path = ips.get(task.node_id);
			fields.push(path?.epochs.get(task.epoch) ?? (path?.currentEpoch === task.epoch ? path.current : undefined));
		}
		return fields.some(field => field?.toLowerCase().includes(query));
	}).sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at) || b.id - a.id);
}

/** 探针队列汇总:在飞(全量,含僵尸可见)+ 近期结论计数(窗口内)。 */
type ProbeSummary = { pending: number; running: number; cancelled: number; clean: number; degraded: number; error: number; inFlight: number };

export function probeSummary(probes: QualityProbeTask[], nowMs: number = Date.now(), windowMs: number = PROBE_RECENT_WINDOW_MS): ProbeSummary {
	const summary: ProbeSummary = { pending: 0, running: 0, cancelled: 0, clean: 0, degraded: 0, error: 0, inFlight: 0 };
	for (const task of probes) {
		if (task.state === "pending" || task.state === "running") {
			if (task.state === "pending") {
				summary.pending += 1;
			} else {
				summary.running += 1;
			}
			// 在飞不设窗:僵尸必须始终可见,而不是滑出列表被美化。
			summary.inFlight += 1;
			continue;
		}
		// 已中止(案件结案/租约回收)不是失败,不计结论三分支。
		// 时间戳不可解析时按近期计(可见优先,与结论分支同口径——
		// 两侧不一致会把坏数据显示成"凭空消失")。
		if (task.state === "cancelled") {
			const at = Date.parse(task.finished_at ?? task.created_at);
			if (!Number.isFinite(at) || nowMs - at <= windowMs) {
				summary.cancelled += 1;
			}
			continue;
		}
		const at = Date.parse(task.finished_at ?? task.created_at);
		if (Number.isFinite(at) && nowMs - at > windowMs) {
			continue;
		}
		if (probeResult(task) === "clean") {
			summary.clean += 1;
		} else if (probeResult(task) === "degraded") {
			summary.degraded += 1;
		} else {
			summary.error += 1;
		}
	}
	return summary;
}

/** 探针耗时(完成-创建)毫秒;未完成为 null。 */
export function probeDurationMs(task: QualityProbeTask): number | null {
	if (!task.finished_at) {
		return null;
	}
	const finished = Date.parse(task.finished_at);
	const created = Date.parse(task.created_at);
	if (!Number.isFinite(finished) || !Number.isFinite(created)) {
		return null;
	}
	return Math.max(0, finished - created);
}


export function getProbeFinding(
	task: QualityProbeTask,
	t: (key: string, opts?: Record<string, unknown>) => string,
): { text: string; tone: "ok" | "bad" | "warn" | "neutral"; badge: string } {
	const finding = (text: string, badge: string, tone: "ok" | "bad" | "warn" | "neutral") => ({
		text: t(`guardProbes.${text}`), badge: t(`guardProbes.${badge}`), tone,
	});
	if (task.state === "pending") return finding("statePending", "statePending", "neutral");
	if (task.state === "running") return finding("stateRunning", "stateRunning", "neutral");
	if (task.state === "cancelled") return finding("stateCancelled", "stateCancelled", "neutral");
	{
		if (task.state === "failed") return finding("proofInterrupted", "resultError", "warn");
		const result = probeResult(task);
		if (result === "clean") return finding("proofNormal", "proofNormalBadge", "ok");
		if (result === "degraded") {
			const bad = new Set(task.proof?.results?.filter(proof => proof.outcome === "degraded").map(proof => proof.kind));
			return finding("proofAbnormal", bad.size > 1 ? "bothAbnormalBadge" : bad.has("account") ? "accountAbnormalBadge" : "exitAbnormalBadge", "bad");
		}
		return finding("proofInconclusive", "proofInconclusiveBadge", "warn");
	}
}
