import { apiRequest } from "@/shared/api/client";
import { createValidatedDecoder, hasShape, isArrayOf, isBoolean, isNumber, isObject, isOptional, isString } from "@/shared/api/decoder";

// 质量层管理面:冻结案件/裁决流/评估/探针队列/
// 节点质量面/人工解禁/批量轮换。对应后端 qualityhttp handler。

export type QualityCaseLive = {
	assessment?: ExperimentReport;
	case_id: number;
	defendant: number;
	account_degraded_exits: number;
	account_need_exits: number;
	account_span_nodes: number;
	account_need_span_nodes: number;
	exit_witnesses: number;
	exit_need_n: number;
	exit_degraded: number;
	exit_need_k: number;
	differential_valid?: number;
	differential_failed?: number;
	differential_transport_failed?: number;
	differential_attempts?: number;
	differential_attempt_limit?: number;
	jury_failed?: number;
	pending_probes: number;
	waiting_reason: string;
	/** 稳定机器码(deadline_reached/awaiting_probes/...),面板按码翻译;
	 * 旧后端无此字段时回退 waiting_reason 原文。 */
	waiting_reason_code?: string;
	deadline_at?: string;
	expired?: boolean;
};

export type QualityCase = {
	id: number;
	status: string;
	verdict: string;
	opened_at: string;
	closed_at?: string | null;
	parties: Array<{
		kind: string;
		account_id: number;
		node_id: number;
		epoch: number;
		role: string;
		disposition: string;
	}>;
	evidence?: Record<string, unknown>;
	live?: QualityCaseLive;
};

export type QualityReviewStats = {
	opened: number;
	convicted: number;
	dismissed: number;
	withdrawn: number;
	retried?: number;
};

export type ProbeExperiment = {
	version: string;
	trigger_event_id: string;
	sample: string;
	baseline: { model: string; revision: number; rule_version: string;
		profile: { known: boolean; protocol?: string; reasoning_effort?: string; tools: boolean } };
};

export type QualityProbeTask = {
	experiment?: ProbeExperiment;
	control_account_id?: number;
	control_node_id?: number;
	control_epoch?: number;
	failure_kind?: string;
	path_key?: string;
	control_outcome?: string;
	control_detail?: string;
	control_path_key?: string;
	control_verified?: boolean;
	id: number;
	case_id: number;
	direction: string;
	defendant: number;
	node_id: number;
	epoch: number;
	/** Original degraded path used by attempt 1; node_id/epoch is attempt 2. */
	baseline_node_id?: number;
	baseline_epoch?: number;
	verified_ip_change?: boolean;
	juror: number;
	state: string;
	result: string;
	detail: string;
	created_at: string;
	finished_at?: string | null;
};

export type ExperimentGroup = {
	attempts: number; pending: number; cancelled: number; clean: number; degraded: number;
	transport: number; unavailable: number; confirmed_degraded: number; confirmed_transport: number;
};
export type ExperimentReport = {
	policy: { experiment?: ProbeExperiment; version: string; account_paths: number; account_nodes: number; jury_size: number; jury_degraded: number;
		transport_paths: number; max_account_attempts: number; max_jury_attempts: number; deadline_at: string };
	verdict: string; reason: string; phase: string; account_suspicion: string;
	account: ExperimentGroup; exit: ExperimentGroup;
	account_support: string[]; exit_support: string[]; limitations: string[];
};

export type QualityNodeView = {
	node_id: number;
	current_epoch: number;
	current_ip: string;
	state: string;
	webhook: boolean;
	degrade_total: number;
	degrade_detail: Array<{
		epoch: number;
		ip: string;
		count: number;
		first_at: string;
		last_at: string;
	}>;
};

// 批8 修复:后端列表端点的 data 是 {items:[...]} 包络——此前解码器
// 期望正确的响应包络,解码失败→查询静默失败→冻结案件永远为空(
// 上线起从未显示过数据)。
export const caseDecoder = createValidatedDecoder<{ items: QualityCase[] }>("quality cases", hasShape({
	items: isArrayOf(hasShape({
		id: isNumber,
		status: isString,
		verdict: isString,
		opened_at: isString,
		closed_at: isOptional(isString),
		parties: isArrayOf(hasShape({
			kind: isString,
			account_id: isNumber,
			node_id: isNumber,
			epoch: isNumber,
			role: isString,
			disposition: isString,
		})),
		evidence: isOptional(isObject),
		live: isOptional(isObject),
	})),
}));

export const probeDecoder = createValidatedDecoder<{ items: QualityProbeTask[] }>("quality probes", hasShape({
	items: isArrayOf(hasShape({
		id: isNumber,
		case_id: isNumber,
		direction: isString,
		defendant: isNumber,
		node_id: isNumber,
		epoch: isNumber,
		baseline_node_id: isOptional(isNumber),
		baseline_epoch: isOptional(isNumber),
		verified_ip_change: isOptional(isBoolean),
		juror: isNumber,
		state: isString,
		result: isString,
		detail: isString,
		created_at: isString,
		finished_at: isOptional(isString),
	})),
}));

export const nodeDecoder = createValidatedDecoder<{ nodes: QualityNodeView[] }>("quality nodes", hasShape({
	nodes: isArrayOf(hasShape({
		node_id: isNumber,
		current_epoch: isNumber,
		current_ip: isString,
		state: isString,
		webhook: isBoolean,
		degrade_total: isNumber,
		degrade_detail: isArrayOf(hasShape({
			epoch: isNumber,
			ip: isString,
			count: isNumber,
			first_at: isString,
			last_at: isString,
		})),
	})),
}));

export function fetchQualityCases(signal?: AbortSignal): Promise<QualityCase[]> {
	return apiRequest("/api/admin/v1/quality/court/cases", { signal }, caseDecoder).then((value) => value.items);
}

export function fetchQualityProbes(signal?: AbortSignal, limit = 50): Promise<QualityProbeTask[]> {
	return apiRequest(`/api/admin/v1/quality/probes?limit=${limit}`, { signal }, probeDecoder).then((value) => value.items);
}

// 案件级探针历史:返回该案件的全部任务,不受全局最近窗口截断——
// 已结案件的调查过程必须永远可读,否则详情会退化成"无探针"的黑盒结论。
export function fetchQualityProbesForCase(signal: AbortSignal | undefined, caseId: number): Promise<QualityProbeTask[]> {
	return apiRequest(`/api/admin/v1/quality/probes?case_id=${caseId}`, { signal }, probeDecoder).then((value) => value.items);
}

export function fetchQualityNodes(signal?: AbortSignal): Promise<QualityNodeView[]> {
	return apiRequest("/api/admin/v1/quality/egress", { signal }, nodeDecoder).then((value) => value.nodes);
}

export function triggerQualityReview(): Promise<QualityReviewStats> {
	const decoder = createValidatedDecoder<QualityReviewStats>("quality review", hasShape({
		opened: isNumber,
		convicted: isNumber,
		dismissed: isNumber,
		withdrawn: isNumber,
		retried: isOptional(isNumber),
	}));
	return apiRequest("/api/admin/v1/quality/court/review", { method: "POST" }, decoder);
}

export function releaseQualityCase(caseId: number, reason: string): Promise<{ released: boolean }> {
	return apiRequest(`/api/admin/v1/quality/court/cases/${caseId}/release`, { method: "POST", body: { reason } },
		createValidatedDecoder("quality case release", hasShape({ released: isBoolean })));
}

export type QualityGuardPolicy = {
 evidence_timeout: string; created_timeout: string;
 admission_timeout: string; tool_admission_timeout: string;
 account_cooldown: string; idle_account_cooldown: string;
 enabled: boolean; guarded_models: string[]; max_attempts: number;
 reasoning_expected: boolean; exhaustion_policy: string;
};
export type QualityGuardConfig = QualityGuardPolicy & {
 revision: string;
 file_defaults?: QualityGuardPolicy;
 self_check: { outcome: string; detail?: string };
};
const guardPolicyShape = {
 evidence_timeout: isString, created_timeout: isString,
 admission_timeout: isString, tool_admission_timeout: isString,
 account_cooldown: isString, idle_account_cooldown: isString,
 enabled: isBoolean, guarded_models: isArrayOf(isString), max_attempts: isNumber,
 reasoning_expected: isBoolean, exhaustion_policy: isString,
};
const guardWireDecoder = createValidatedDecoder<Omit<QualityGuardConfig, "revision"> & { revision: string | number }>("quality guard", hasShape({
 ...guardPolicyShape,
 revision: (value) => typeof value === "string" ? /^\d+$/.test(value) : typeof value === "number" && Number.isSafeInteger(value) && value >= 0,
 file_defaults: isOptional(hasShape(guardPolicyShape)),
 self_check: hasShape({ outcome: isString, detail: isOptional(isString) }),
}));
const guardConfigDecoder = (value: unknown): QualityGuardConfig => {
 const decoded = guardWireDecoder(value);
 return { ...decoded, revision: String(decoded.revision) };
};
export function fetchQualityGuard(signal?: AbortSignal): Promise<QualityGuardConfig> {
 return apiRequest("/api/admin/v1/quality/guard", { signal }, guardConfigDecoder);
}
export type QualityGuardInput = Pick<QualityGuardConfig, "revision" | "enabled" | "guarded_models"> & Partial<Pick<QualityGuardConfig,
 "max_attempts" | "evidence_timeout" | "created_timeout" | "admission_timeout" | "tool_admission_timeout" | "account_cooldown" | "idle_account_cooldown">>;

export function updateQualityGuard(input: QualityGuardInput, signal?: AbortSignal): Promise<QualityGuardConfig> {
 return apiRequest("/api/admin/v1/quality/guard", { method: "PUT", body: input, signal }, guardConfigDecoder);
}
export function resetQualityGuard(revision: string, signal?: AbortSignal): Promise<QualityGuardConfig> {
 return apiRequest("/api/admin/v1/quality/guard", { method: "DELETE", body: { revision }, signal }, guardConfigDecoder);
}

export function unbanQualityNode(nodeID: number): Promise<boolean> {
	const decoder = createValidatedDecoder<{ unbanned: boolean }>("quality unban", hasShape({ unbanned: isBoolean }));
	return apiRequest("/api/admin/v1/quality/egress/unban", {
		method: "POST",
		body: JSON.stringify({ node_id: nodeID }),
	}, decoder).then((value) => value.unbanned);
}

export type QualitySettingsInput = {
	account_need_exits: number;
	account_span_nodes: number;
	exit_need_n: number;
	exit_need_k: number;
	differential_exits: number;
	jurors_per_exit: number;
	probe_budget: number;
	retention: string;
	evidence_window: string;
	investigation_timeout: string;
};

export type QualitySettings = QualitySettingsInput & {
 max_rotations_per_hour: number;
 revision: string;
 applied_revision: string;
 apply_pending: boolean;
 apply_error: string;
 applied: QualitySettingsInput;
};

const qualityInputShape = {
 account_need_exits: isNumber, account_span_nodes: isNumber,
 exit_need_n: isNumber, exit_need_k: isNumber,
 differential_exits: isNumber, jurors_per_exit: isNumber, probe_budget: isNumber,
 retention: isString, evidence_window: isString, investigation_timeout: isString,
};

export const settingsDecoder = createValidatedDecoder<QualitySettings>("quality settings", hasShape({
 ...qualityInputShape,
 max_rotations_per_hour: isNumber,
 revision: isString,
 applied_revision: isString,
 apply_pending: isBoolean,
 apply_error: isString,
 applied: hasShape(qualityInputShape),
}));

export function fetchQualitySettings(signal?: AbortSignal): Promise<QualitySettings> {
	return apiRequest("/api/admin/v1/quality/settings", { signal }, settingsDecoder);
}

// Only owned editable fields and the version observed when editing are written.
export function qualitySettingsWrite(input: QualitySettings) {
 return {
  account_need_exits: input.account_need_exits, account_span_nodes: input.account_span_nodes,
  exit_need_n: input.exit_need_n, exit_need_k: input.exit_need_k,
  differential_exits: input.differential_exits, jurors_per_exit: input.jurors_per_exit,
  probe_budget: input.probe_budget, retention: input.retention,
  evidence_window: input.evidence_window, investigation_timeout: input.investigation_timeout,
  revision: input.revision,
 };
}

export function updateQualitySettings(input: QualitySettings, signal?: AbortSignal): Promise<QualitySettings> {
	return apiRequest("/api/admin/v1/quality/settings", {
		method: "PUT",
		body: JSON.stringify(qualitySettingsWrite(input)),
		signal,
	}, settingsDecoder);
}
