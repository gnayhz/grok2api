import type { QualityCase, QualityCaseLive, QualityNodeView, QualityProbeTask } from "@/entities/guard/quality-api";
// 质量防护监控台的视图模型(纯函数层)。见文件尾。
// 导出缝说明:未被运行时组件消费、仅被 quality-view.test.ts 引用的
// 导出(如 splitCases/verdictNarrative/waitingReasonKey)是测试缝——
// 纯函数契约由离线夹具测试锁定,保留导出以便测试直接驱动。

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

// 质量防护监控台的视图模型(纯函数层)。
// 输入=质量层 API 解码结果(形状由契约测试对真实部署夹具锁定),
// 输出=组件直接渲染的派生结构。全部无副作用、时间以参数注入,
// 离线夹具即可完整测试——"面板显示逻辑"不再只靠人肉点开验证。

/** 在审(羁押中)/已结(裁决流)分组。 */
export function splitCases(cases: QualityCase[]): { open: QualityCase[]; closed: QualityCase[] } {
	const open: QualityCase[] = [];
	const closed: QualityCase[] = [];
	for (const item of cases) {
		if (item.status === "investigating") {
			open.push(item);
		} else {
			closed.push(item);
		}
	}
	return { open, closed };
}

/** 在押账号数(在审案件中去重的羁押被告)。 */
export function heldAccountCount(open: QualityCase[]): number {
	const ids = new Set<number>();
	for (const item of open) {
		for (const party of item.parties) {
			if (party.kind === "account" && party.disposition === "remanded") {
				ids.add(party.account_id);
			}
		}
	}
	return ids.size;
}

/** 在押出口数(在审案件中去重的羁押出口)。 */
export function heldExitCount(open: QualityCase[]): number {
	const ids = new Set<number>();
	for (const item of open) {
		for (const party of item.parties) {
			if (party.kind === "exit" && party.disposition === "remanded") {
				ids.add(party.node_id);
			}
		}
	}
	return ids.size;
}

/** Go duration 字符串("45m"/"12h"/"45m0s" 复合段)→毫秒;无法解析返回 null。 */
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

/** 证据进度条:当前值/门槛/是否达标。 */
type Meter = { current: number; target: number; fraction: number; met: boolean };

function meter(current: number, target: number): Meter {
	const safeTarget = target > 0 ? target : 1;
	return {
		current,
		target,
		fraction: Math.min(1, Math.max(0, current / safeTarget)),
		met: target > 0 && current >= target,
	};
}

/** 案件触发的证据规则指纹(立案证据面里的 rule/trigger 字段)。 */
export function caseTrigger(item: QualityCase): string {
	const evidence = item.evidence;
	if (!evidence) {
		return "";
	}
	const candidate = evidence.trigger ?? evidence.rule;
	return typeof candidate === "string" ? candidate : "";
}

/** 结案/在审案件的证据摘要(裁决流"依据"行):从证据 JSON 提取可读字段。 */
type EvidenceDigest = {
	trigger: string;
	degradedExits: number | null;
	spanNodes: number | null;
	witnessExits: number | null;
	defendant: number | null;
	exitNode: number | null;
	auxiliary: string | null;
	differentialValid: number | null;
	differentialFailed: number | null;
	differentialTransportFailed: number | null;
	differentialAttempts: number | null;
	juryFailed: number | null;
};

export function evidenceDigest(item: QualityCase): EvidenceDigest | null {
	const evidence = item.evidence;
	if (!evidence) {
		return null;
	}
	const num = (value: unknown): number | null => (typeof value === "number" ? value : null);
	const est = evidence.account_est;
	const estRecord = est && typeof est === "object" ? (est as Record<string, unknown>) : null;
	const exit = evidence.exit;
	const exitRecord = exit && typeof exit === "object" ? (exit as Record<string, unknown>) : null;
	const differentialClean = num(evidence.diff_clean);
	const differentialDegraded = num(evidence.diff_degraded);
	return {
		trigger: caseTrigger(item),
		// Opening estimates are present only on an investigating case. Closed
		// direct-loop cases store the finite round counters at the top level.
		degradedExits: differentialDegraded ?? num(evidence.degraded_exits) ?? (estRecord ? num(estRecord.degraded_exits) : null),
		spanNodes: num(evidence.diff_span_nodes) ?? num(evidence.span_nodes) ?? (estRecord ? num(estRecord.span_nodes) : null),
		witnessExits: num(evidence.jury_total) ?? num(evidence.witnesses) ?? (estRecord ? num(estRecord.witness_exits) : null),
		defendant: num(evidence.defendant),
		exitNode: exitRecord ? num(exitRecord.node) : null,
		auxiliary: typeof evidence.auxiliary === "string" && evidence.auxiliary.length > 0 ? evidence.auxiliary : null,
		differentialValid: num(evidence.diff_valid) ?? (differentialClean !== null && differentialDegraded !== null ? differentialClean + differentialDegraded : null),
		differentialFailed: num(evidence.diff_failed),
		differentialTransportFailed: num(evidence.diff_transport_failed),
		differentialAttempts: num(evidence.diff_tasks),
		juryFailed: num(evidence.jury_failed),
	};
}

/** 实时证据面 → 四条定罪进度(账号侧两条,出口侧两条)。 */
type LiveMeters = {
	account: Array<{ key: "degradedExits" | "spanNodes"; meter: Meter }>;
	exit: Array<{ key: "witnesses" | "degradedVotes"; meter: Meter }>;
	pendingProbes: number;
	waitingReason: string;
	differentialValid: number | null;
	differentialFailed: number | null;
	differentialTransportFailed: number | null;
	differentialAttempts: number | null;
	differentialAttemptLimit: number | null;
	juryFailed: number | null;
};

export function liveMeters(live: QualityCaseLive | undefined): LiveMeters | null {
	if (!live) {
		return null;
	}
	return {
		account: [
			{ key: "degradedExits", meter: meter(live.account_degraded_exits, live.account_need_exits) },
			{ key: "spanNodes", meter: meter(live.account_span_nodes, live.account_need_span_nodes) },
		],
		exit: [
			{ key: "witnesses", meter: meter(live.exit_witnesses, live.exit_need_n) },
			{ key: "degradedVotes", meter: meter(live.exit_degraded, live.exit_need_k) },
		],
		pendingProbes: live.pending_probes,
		waitingReason: live.waiting_reason,
		differentialValid: typeof live.differential_valid === "number" ? live.differential_valid : null,
		differentialFailed: typeof live.differential_failed === "number" ? live.differential_failed : null,
		differentialTransportFailed: typeof live.differential_transport_failed === "number" ? live.differential_transport_failed : null,
		differentialAttempts: typeof live.differential_attempts === "number" ? live.differential_attempts : null,
		differentialAttemptLimit: typeof live.differential_attempt_limit === "number" ? live.differential_attempt_limit : null,
		juryFailed: typeof live.jury_failed === "number" ? live.jury_failed : null,
	};
}

/** 探针近期结论统计窗:与证据窗口对齐——旧结论随窗口滑出归零,
 * 不允许"无新活动但数字常驻"(清态事故的显示侧教训)。 */
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

/** 案件的调查动作分组:账号差分(对比账户)与出口陪审(对比 IP)。 */
type CaseProbes = { account: QualityProbeTask[]; exit: QualityProbeTask[] };

export function groupProbesByCase(probes: QualityProbeTask[]): Map<number, CaseProbes> {
	const grouped = new Map<number, CaseProbes>();
	for (const task of probes) {
		if (task.direction === "case_proof") continue;
		const entry = grouped.get(task.case_id) ?? { account: [], exit: [] };
		if (task.direction === "account_differential") {
			entry.account.push(task);
		} else {
			entry.exit.push(task);
		}
		grouped.set(task.case_id, entry);
	}
	return grouped;
}

/** 案件调查槽位:槽数至少为后台配置(差分对照出口数/每出口陪审员数),
 * 每槽=一个派出的目标(差分按对照出口节点键控,陪审按陪审员账号键控);
 * 同槽重派(IP 失效换 IP 再试)只更新该槽最新结论——位子不变,结果
 * 更新;有界替代会扩展槽位以保留每个新目标,不会无限堆圆点。未填槽=待派(hollow)。
 */
type ProbeSlot = { task: QualityProbeTask | null; key: string };

export function slotProbes(tasks: QualityProbeTask[], keyOf: (task: QualityProbeTask) => string, slots: number): ProbeSlot[] {
	const latestByKey = new Map<string, QualityProbeTask>();
	for (const task of [...tasks].sort((a, b) => a.id - b.id)) {
		latestByKey.set(keyOf(task), task);
	}
	const filled: ProbeSlot[] = [...latestByKey.entries()]
		.sort((a, b) => b[1].id - a[1].id)
		.map(([key, task]) => ({ task, key }));
	const result: ProbeSlot[] = filled.slice(0, Math.max(0, slots));
	while (result.length < slots) {
		result.push({ task: null, key: "empty-" + result.length });
	}
	return result;
}

/** 差分槽键:对照出口的节点+IP epoch,避免同节点翻 epoch 互相覆盖。 */
export function differentialSlotKey(task: QualityProbeTask): string {
	return "node-" + task.node_id + "-epoch-" + task.epoch;
}

/** 陪审槽键:陪审员账号。 */
export function jurySlotKey(task: QualityProbeTask): string {
	return "juror-" + task.juror;
}

/** 调查圆点色调:绿=干净,红=降智,琥珀=在途,灰=失败/不可采。 */
export function probeDotTone(task: QualityProbeTask): "ok" | "bad" | "pending" | "muted" {
	if (task.state === "pending" || task.state === "running") {
		return "pending";
	}
	// cancelled(案件结案/租约回收)无结论,灰点。
	if (task.state === "cancelled") {
		return "muted";
	}
	if (task.result === "clean") {
		return "ok";
	}
	if (task.result === "degraded") {
		return "bad";
	}
	return "muted";
}

/** 案件当事方摘要:被告账号 + 涉案出口(节点/epoch)。 */
export function casePartySummary(item: QualityCase): { defendantAccounts: number[]; exits: Array<{ node: number; epoch: number }> } {
	const defendantAccounts: number[] = [];
	const exits: Array<{ node: number; epoch: number }> = [];
	for (const party of item.parties) {
		if (party.kind === "account" && !defendantAccounts.includes(party.account_id)) {
			defendantAccounts.push(party.account_id);
		} else if (party.kind === "exit" && !exits.some((exit) => exit.node === party.node_id)) {
			exits.push({ node: party.node_id, epoch: party.epoch });
		}
	}
	return { defendantAccounts, exits };
}

/** 处置 → 徽章色调。 */
export function dispositionTone(disposition: string): "destructive" | "warning" | "ok" | "muted" {
	if (disposition === "remanded" || disposition === "sentenced") {
		return "destructive";
	}
	if (disposition === "released" || disposition === "dismissed") {
		return "ok";
	}
	return "muted";
}

/** 裁决 → 徽章色调。与 quality-tribunal-view.tsx 里同名但按案件对象
 * 取值的 verdictTone 不同,这里是纯字符串状态的测试缝投影。 */
export function testCaseVerdictTone(status: string): "destructive" | "warning" | "ok" | "muted" {
	if (status === "account_guilty" || status === "both_guilty") {
		return "destructive";
	}
	if (status === "exit_guilty") {
		return "warning";
	}
	if (status === "dismissed") {
		return "ok";
	}
	if (status === "insufficient") {
		return "warning";
	}
	return "muted";
}

/** A task outcome is a measurement; causal attribution belongs to the case report. */
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
	if (task.direction === "case_proof") {
		if (task.state === "failed") return finding("proofInterrupted", "resultError", "warn");
		const result = probeResult(task);
		if (result === "clean") return finding("proofNormal", "proofNormalBadge", "ok");
		if (result === "degraded") {
			const bad = new Set(task.proof?.results?.filter(proof => proof.outcome === "degraded").map(proof => proof.kind));
			return finding("proofAbnormal", bad.size > 1 ? "bothAbnormalBadge" : bad.has("account") ? "accountAbnormalBadge" : "exitAbnormalBadge", "bad");
		}
		return finding("proofInconclusive", "proofInconclusiveBadge", "warn");
	}
	if (task.state === "done" && task.result === "clean") return finding("findingClean", "resultClean", "ok");
	if (task.state === "done" && task.result === "degraded") return finding("findingDegraded", "resultDegraded", "bad");

	const kind = task.failure_kind || "";
	const detail = task.detail || "";
	// Typed provenance takes precedence over legacy diagnostic text. In particular,
	// transport_error_not_evidence is a disclaimer, not a transport failure kind.
	if (kind.startsWith("local/verification/") || (!kind && /(?:epoch_stale|identity_mismatch|path_unverified)/.test(detail))) {
		return finding("findingUnverified", "resultUnverified", "warn");
	}
	if (kind === "upstream/admission/created_timeout" || kind === "created_timeout" || (!kind && detail.includes("created_timeout"))) {
		return finding("findingCreatedTimeout", "resultCreatedTimeout", "warn");
	}
	if (kind === "upstream/admission/evidence_timeout" || kind === "evidence_timeout" || (!kind && detail.includes("evidence_timeout"))) {
		return finding("findingEvidenceTimeout", "resultEvidenceTimeout", "warn");
	}
	if (kind.startsWith("local/")) return finding("findingUnavailable", "resultUnavailable", "warn");
	if (kind.startsWith("upstream/headers/") || (!kind && detail.includes("upstream_http"))) {
		return finding("findingUpstreamHttp", "resultUpstreamHttp", "warn");
	}
	if (kind === "upstream/admission/truncated_stream" || kind === "transport" || (!kind && /^(?:transport_error|transport:)/.test(detail))) {
		return finding("findingTransportError", "resultTransport", "warn");
	}
	if (task.result === "error" || task.state === "failed") return finding("findingFailed", "resultError", "warn");
	return finding("findingUnknown", "stateDone", "neutral");
}


/** 案件探针行 → 与裁决同口径的计数(clean/degraded 只数可采结论,
 * 与后端 summarizeSimpleProbes 的 admissibility 对齐:陪审 done 即票;
 * 差分 clean 即票,degraded 必须验证过 IP 变化;失败/取消不算票)。
 * 案件级全量任务(不受全局窗口截断)是数据源,保证历史案件叙事不缺数。 */
type CaseProbeTally = {
	juryTotal: number;
	juryClean: number;
	juryDegraded: number;
	diffValid: number;
	diffClean: number;
	diffDegraded: number;
	pending: number;
};

export function tallyCaseProbes(probes: { account: QualityProbeTask[]; exit: QualityProbeTask[] }): CaseProbeTally {
	const tally: CaseProbeTally = { juryTotal: 0, juryClean: 0, juryDegraded: 0, diffValid: 0, diffClean: 0, diffDegraded: 0, pending: 0 };
	for (const task of probes.exit) {
		if (task.state === "pending" || task.state === "running") {
			tally.pending += 1;
			continue;
		}
		if (task.state !== "done") {
			continue;
		}
		if (task.result === "clean") {
			tally.juryTotal += 1;
			tally.juryClean += 1;
		} else if (task.result === "degraded") {
			tally.juryTotal += 1;
			tally.juryDegraded += 1;
		}
	}
	for (const task of probes.account) {
		if (task.state === "pending" || task.state === "running") {
			tally.pending += 1;
			continue;
		}
		if (task.state !== "done") {
			continue;
		}
		if (task.result === "clean") {
			tally.diffValid += 1;
			tally.diffClean += 1;
		} else if (task.result === "degraded" && task.verified_ip_change) {
			tally.diffValid += 1;
			tally.diffDegraded += 1;
		}
	}
	return tally;
}

/** 裁决叙事:把对照实验结果翻译成"为什么支持账号/出口/无法区分"的一句话。
 * 输入的 tally 来自案件全量探针行(带真实身份显示名);数据不足时返回
 * null,由调用方回退到通用文案。纯函数,离线夹具可测。 */
type VerdictNarrative = { key: string; params: Record<string, string | number> };

export function verdictNarrative(input: {
	verdict: string;
	status: string;
	rule: string;
	reason: string;
	defendantLabel: string;
	exitLabel: string;
	tally: CaseProbeTally | null;
	juryNeedN: number | null;
	diffNeed: number | null;
	diffTransportFailed: number | null;
	thresholdPercent: number | null;
}): VerdictNarrative | null {
	if (!input.tally) {
		return null;
	}
	const defendant = input.defendantLabel || "—";
	const exit = input.exitLabel || "—";
	const threshold = input.thresholdPercent === null ? 90 : Math.round(input.thresholdPercent);
	const juryNeedN = input.juryNeedN ?? 0;
	const diffNeed = input.diffNeed ?? 0;
	if (input.status === "investigating") {
		return {
			key: "quality.tribunal.narrative.investigating",
			params: {
				exit,
				juryTotal: input.tally.juryTotal,
				juryNeedN,
				diffValid: input.tally.diffValid,
				diffNeed,
			},
		};
	}
	switch (input.verdict) {
	case "exit_guilty":
		return {
			key: "quality.tribunal.narrative.exitGuilty",
			params: { exit, juryDegraded: input.tally.juryDegraded, juryTotal: input.tally.juryTotal },
		};
	case "account_guilty":
		if (input.rule === "account_transport_pattern_conviction") {
			return {
				key: "quality.tribunal.narrative.accountTransportPattern",
				params: {
					exit, defendant,
					juryClean: input.tally.juryClean, juryTotal: input.tally.juryTotal,
					transportFailed: input.diffTransportFailed ?? input.tally.diffValid,
				},
			};
		}
		if (input.rule === "account_sso_conviction") {
			return {
				key: "quality.tribunal.narrative.accountSSO",
				params: { defendant, exit },
			};
		}
		return {
			key: "quality.tribunal.narrative.accountDifferential",
			params: {
				exit, defendant,
				juryClean: input.tally.juryClean, juryTotal: input.tally.juryTotal,
				diffDegraded: input.tally.diffDegraded,
			},
		};
	case "insufficient":
		if (input.reason.startsWith("investigation_timeout")) {
			return {
				key: "quality.tribunal.narrative.insufficientTimeout",
				params: { juryTotal: input.tally.juryTotal, diffValid: input.tally.diffValid, threshold },
			};
		}
		return {
			key: "quality.tribunal.narrative.insufficient",
			params: {
				juryTotal: input.tally.juryTotal, juryNeedN,
				diffValid: input.tally.diffValid, diffNeed, threshold,
			},
		};
	default:
		return null;
	}
}

/** 等待原因码 → 面板 i18n key;未知码/无码时返回 null(调用方回退原文)。 */
export function waitingReasonKey(code: string | undefined): string | null {
	switch (code) {
	case "deadline_reached":
	case "awaiting_probes":
	case "replacing_failed_exits":
	case "awaiting_supplement":
	case "probes_settled":
		return "quality.tribunal.waitingReason." + code;
	default:
		return null;
	}
}

/** 出口质量行视图:按累计降智排序(重的在前)。 */
export function sortedNodes(nodes: QualityNodeView[]): QualityNodeView[] {
	return [...nodes].sort((a, b) => b.degrade_total - a.degrade_total || a.node_id - b.node_id);
}
