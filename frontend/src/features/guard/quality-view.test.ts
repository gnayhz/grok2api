import { readFileSync } from "node:fs";
import { join } from "node:path";
import { deepStrictEqual, ok, strictEqual } from "node:assert";
import { test } from "node:test";

import { caseDecoder, nodeDecoder, probeDecoder, type QualityCase, type QualityProbeTask } from "./quality-api.ts";
import {
	buildQualityExitIPIndex,
	casePartySummary,
	caseTrigger,
	dispositionTone,
	differentialSlotKey,
	evidenceDigest,
	groupProbesByCase,
	heldAccountCount,
	heldExitCount,
	jurySlotKey,
	liveMeters,
	maskIP,
	parseGoDurationMs,
	probeDotTone,
	probeDurationMs,
	probeFailureNote,
	probeSummary,
	qualityAccountDisplay,
	qualityExitDisplay,
	slotProbes,
	sortedNodes,
	splitCases,
	tallyCaseProbes,
	verdictNarrative,
	verdictTone,
	waitingReasonKey,
} from "./quality-view.ts";

// 视图模型测试:契约测试锁定"解码不炸",这里锁定
// "派生正确"——分组/去重计数/进度条全部对着 8003 真实夹具断言,
// 面板数字的计算路径离线可验,不靠人肉点开页面核对。
//
// 夹具两份:cases.json=纯线上抓取(envelope/分组一致性);cases-open.json
// =真实案件还原为在审形态(在审派生:live 视图/在押去重)——抓取
// 时系统可能恰好健康(零在审),在审断言必须有稳定数据源。

function fixture(name: string): unknown {
	return JSON.parse(readFileSync(join(import.meta.dirname, "__fixtures__", `${name}.json`), "utf8"));
}

function mustCases(): QualityCase[] {
	return caseDecoder(fixture("cases")).items;
}

function mustOpenCases(): QualityCase[] {
	return caseDecoder(fixture("cases-open")).items;
}

test("splitCases 在审/已结分组与后端 status 一致(线上抓取)", () => {
	const { open, closed } = splitCases(mustCases());
	for (const item of open) {
		strictEqual(item.status, "investigating");
	}
	for (const item of closed) {
		ok(item.status !== "investigating");
	}
	strictEqual(open.length + closed.length, caseDecoder(fixture("cases")).items.length);
});

test("在审夹具:全部携带直接闭环 live 视图", () => {
	const open = splitCases(mustOpenCases()).open;
	ok(open.length >= 2, "在审夹具应含多案(去重断言需要)");
	for (const item of open) {
		strictEqual(item.status, "investigating");
		ok(item.live, "在审案件应携带实时证据面");
	}
});

test("在押账号/出口去重计数(两案共享出口只计一次)", () => {
	const open = splitCases(mustOpenCases()).open;
	const accountIds = new Set<number>();
	const exitIds = new Set<number>();
	for (const item of open) {
		for (const party of item.parties) {
			if (party.disposition !== "remanded") {
				continue;
			}
			if (party.kind === "account") {
				accountIds.add(party.account_id);
			} else if (party.kind === "exit") {
				exitIds.add(party.node_id);
			}
		}
	}
	strictEqual(heldAccountCount(open), accountIds.size);
	strictEqual(heldExitCount(open), exitIds.size);
	ok(exitIds.size < open.reduce((sum, item) => sum + item.parties.filter((p) => p.kind === "exit").length, 0), "共享出口场景才验证去重");
});

test("Go duration 解析:单位全覆盖与非法输入", () => {
	strictEqual(parseGoDurationMs("45m"), 45 * 60_000);
	strictEqual(parseGoDurationMs("12h"), 12 * 3_600_000);
	strictEqual(parseGoDurationMs("90s"), 90_000);
	strictEqual(parseGoDurationMs("500ms"), 500);
	strictEqual(parseGoDurationMs("1.5h"), 5_400_000);
	strictEqual(parseGoDurationMs(""), null);
	strictEqual(parseGoDurationMs("45"), null);
	strictEqual(parseGoDurationMs("m45"), null);
});

test("实时证据面进度条:分子分母来自 live,达标判定正确", () => {
	const open = splitCases(mustOpenCases()).open;
	for (const item of open) {
		const meters = liveMeters(item.live);
		ok(meters);
		const [degradedExits, spanNodes] = meters!.account;
		const [witnesses, degradedVotes] = meters!.exit;
		strictEqual(degradedExits.meter.current, item.live!.account_degraded_exits);
		strictEqual(degradedExits.meter.target, item.live!.account_need_exits);
		strictEqual(spanNodes.meter.current, item.live!.account_span_nodes);
		strictEqual(witnesses.meter.current, item.live!.exit_witnesses);
		strictEqual(degradedVotes.meter.target, item.live!.exit_need_k);
		for (const entry of [...meters!.account, ...meters!.exit]) {
			ok(entry.meter.fraction >= 0 && entry.meter.fraction <= 1);
			strictEqual(entry.meter.met, entry.meter.current >= entry.meter.target && entry.meter.target > 0);
		}
		strictEqual(meters!.pendingProbes, item.live!.pending_probes);
		ok(meters!.waitingReason.length > 0, "等待原因应为可读人话");
	}
	// 夹具第二案降票 3/3 → 出口定罪达标(met=true)色变红的数据源
	const second = open[1]!.live!;
	strictEqual(second.exit_degraded >= second.exit_need_k, true);
});

test("案件触发规则指纹:在审夹具带 traffic_degraded", () => {
	for (const item of splitCases(mustOpenCases()).open) {
		strictEqual(caseTrigger(item), "traffic_degraded");
	}
});

test("探针汇总:在飞=待执行+执行中,近期结论守恒(线上抓取)", () => {
	const tasks = probeDecoder(fixture("probes")).items;
	// 统计基点锚定夹具时间之后:夹具是冻结快照,用墙钟会让全部结论
	// 滑出窗口,守恒断言失去意义。
	let anchor = 0;
	for (const task of tasks) {
		const at = Date.parse(task.finished_at ?? task.created_at);
		if (Number.isFinite(at) && at > anchor) anchor = at;
	}
	const summary = probeSummary(tasks, anchor + 60_000);
	let pending = 0;
	let running = 0;
	let settled = 0;
	for (const task of tasks) {
		if (task.state === "pending") pending += 1;
		else if (task.state === "running") running += 1;
		else settled += 1;
	}
	strictEqual(summary.inFlight, pending + running);
	strictEqual(summary.pending, pending);
	strictEqual(summary.running, running);
	strictEqual(summary.clean + summary.degraded + summary.error + summary.cancelled, settled);
});

test("探针汇总:窗口外旧结论与已中止不计失败,cancelled 独立计数", () => {
	const now = Date.now();
	const iso = (offsetMs: number) => new Date(now - offsetMs).toISOString();
	const tasks = [
		{ id: 1, case_id: 1, direction: "exit_jury", defendant: 1, juror: 2, state: "failed", result: "error", detail: "", created_at: iso(2 * 60 * 60 * 1000), finished_at: iso(2 * 60 * 60 * 1000) },
		{ id: 2, case_id: 1, direction: "exit_jury", defendant: 1, juror: 2, state: "failed", result: "error", detail: "", created_at: iso(5 * 60 * 1000), finished_at: iso(5 * 60 * 1000) },
		{ id: 3, case_id: 1, direction: "exit_jury", defendant: 1, juror: 2, state: "cancelled", result: "", detail: "案件已结,取证中止", created_at: iso(5 * 60 * 1000), finished_at: iso(5 * 60 * 1000) },
		{ id: 4, case_id: 1, direction: "exit_jury", defendant: 1, juror: 2, state: "running", result: "", detail: "", created_at: iso(2 * 60 * 60 * 1000) },
	] as unknown as Array<Parameters<typeof probeSummary>[0][number]>;
	const summary = probeSummary(tasks, now);
	strictEqual(summary.error, 1); // 两小时前的失败已滑出窗口
	strictEqual(summary.cancelled, 1); // 中止独立计数,不算失败
	strictEqual(summary.inFlight, 1); // 僵尸在飞不受窗口美化,始终可见
});

test("探针耗时:已完成任务 created→finished 非负(线上抓取)", () => {
	const tasks = probeDecoder(fixture("probes")).items;
	const finished = tasks.filter((task) => task.finished_at !== undefined);
	ok(finished.length > 0, "夹具应含已完成探针");
	for (const task of finished) {
		const duration = probeDurationMs(task);
		ok(duration !== null && duration >= 0);
	}
	for (const task of tasks.filter((task) => task.finished_at === undefined)) {
		strictEqual(probeDurationMs(task), null);
	}
});

test("出口视图:排序重者在前,IP 掩码保两段(线上抓取)", () => {
	const nodes = sortedNodes(nodeDecoder(fixture("egress")).nodes);
	ok(nodes.length > 1);
	for (let i = 1; i < nodes.length; i += 1) {
		ok(nodes[i - 1]!.degrade_total >= nodes[i]!.degrade_total);
	}
	strictEqual(maskIP("198.51.100.7"), "198.51.*.*");
	strictEqual(maskIP("2001:0db8:85a3::8a2e:0370:7334"), "2001:0db8:85…");
});

test("人工身份展示:名称优先且稳定保留账号/出口 ID 与 epoch", () => {
	const account = qualityAccountDisplay(4102, { name: "build-account", email: "build@example.test" });
	strictEqual(account.primary, "build-account");
	strictEqual(account.secondary, "build@example.test · #4102");
	strictEqual(account.title, "build-account · build@example.test · #4102");
	strictEqual(qualityAccountDisplay(9999).primary, "#9999");

	const ipByNode = buildQualityExitIPIndex([{
		node_id: 11,
		current_epoch: 3,
		current_ip: "203.0.113.7",
		state: "available",
		webhook: false,
		degrade_total: 0,
		degrade_detail: [],
	}]);
	const exit = qualityExitDisplay(11, 3, { name: "LAX-003" }, ipByNode);
	strictEqual(exit.primary, "LAX-003");
	strictEqual(exit.secondary, "#11@3 · 203.0.113.7");
	strictEqual(exit.title, "LAX-003 · #11@3 · 203.0.113.7");
	// 没有对应历史 epoch 的 IP 时不能把当前 IP 冒充成历史 IP。
	strictEqual(qualityExitDisplay(11, 2, { name: "LAX-003" }, ipByNode).secondary, "#11@2");
});

test("结案证据摘要:字段提取与缺席容忍(线上抓取)", () => {
	const items = caseDecoder(fixture("cases")).items;
	const withEvidence = items.filter((item) => item.evidence !== undefined);
	ok(withEvidence.length > 0, "夹具应含带证据面的案件");
	for (const item of withEvidence) {
		const digest = evidenceDigest(item);
		ok(digest, "带证据面应有摘要");
		strictEqual(digest!.trigger, "traffic_degraded");
		ok(digest!.degradedExits !== null, "account_est.degraded_exits 应可读");
		ok(digest!.spanNodes !== null);
		strictEqual(typeof digest!.defendant, "number");
	}
	strictEqual(evidenceDigest({ ...items[0]!, evidence: undefined }), null);
});

test("探针按案件分组:差分归账号列,陪审归出口列(线上抓取)", () => {
	const tasks = probeDecoder(fixture("probes")).items;
	const grouped = groupProbesByCase(tasks);
	ok(grouped.size > 0, "夹具应含多案件的探针");
	for (const [caseID, entry] of grouped) {
		for (const task of entry.account) {
			strictEqual(task.case_id, caseID);
			strictEqual(task.direction, "account_differential");
		}
		for (const task of entry.exit) {
			strictEqual(task.case_id, caseID);
			strictEqual(task.direction, "exit_jury");
		}
	}
});

test("调查圆点色调:四态稳定", () => {
	const running: QualityProbeTaskLike = { state: "running", result: "" };
	strictEqual(probeDotTone(running as never), "pending");
	const clean: QualityProbeTaskLike = { state: "done", result: "clean" };
	strictEqual(probeDotTone(clean as never), "ok");
	const degraded: QualityProbeTaskLike = { state: "done", result: "degraded" };
	strictEqual(probeDotTone(degraded as never), "bad");
	const failed: QualityProbeTaskLike = { state: "failed", result: "error" };
	strictEqual(probeDotTone(failed as never), "muted");
});

type QualityProbeTaskLike = { state: string; result: string };

test("案件当事方摘要:被告账号与出口去重(在审夹具)", () => {
	for (const item of caseDecoder(fixture("cases-open")).items) {
		const summary = casePartySummary(item);
		ok(summary.defendantAccounts.length >= 1, "在审案件应有被告账号");
		ok(summary.exits.length >= 1, "在审案件应有涉案出口");
		ok(new Set(summary.defendantAccounts).size === summary.defendantAccounts.length, "被告账号应去重");
		ok(new Set(summary.exits.map((exit) => exit.node)).size === summary.exits.length, "出口按节点去重");
	}
});

test("槽位制圆点:槽数=配置,同槽重派取最新,未填=空槽", () => {
	const tasks = probeDecoder(fixture("probes")).items.filter((task) => task.direction === "account_differential");
	ok(tasks.length > 0, "夹具应含差分探针");
	const byCase = groupProbesByCase(tasks);
	const caseTasks = [...byCase.values()][0]!.account;
	const slots = slotProbes(caseTasks, differentialSlotKey, 3);
	strictEqual(slots.length, 3, "槽数恒等于配置");
	const keys = new Set(slots.filter((slot) => slot.task).map((slot) => slot.key));
	strictEqual(keys.size, slots.filter((slot) => slot.task).length, "同键只占一槽");
	// 重派场景:同目标两条,槽内只保留 id 较大的最新结论
	const target = caseTasks[0]!;
	const older = { ...target, id: target.id - 1 };
	const newer = { ...target, id: target.id + 1, result: "clean" };
	const two = slotProbes([older, newer], differentialSlotKey, 3);
	const filled = two.find((slot) => slot.key === differentialSlotKey(newer));
	ok(filled?.task);
	strictEqual(filled!.task!.id, newer.id, "同槽取最新");
	// 空槽补齐
	const sparse = slotProbes([target], jurySlotKey, 4);
	strictEqual(sparse.length, 4);
	strictEqual(sparse.filter((slot) => slot.task === null).length, 3, "未填槽=空");
	strictEqual(differentialSlotKey({ ...target, epoch: target.epoch + 1 }), "node-" + target.node_id + "-epoch-" + (target.epoch + 1), "epoch 变化必须占独立槽");
});

test("失败归因:底层详情→人话键稳定", () => {
	strictEqual(probeFailureNote({ detail: "cross-face probe rejected: build probe on non-build account | x" } as never), "probeNote.crossFace");
	strictEqual(probeFailureNote({ detail: "attempt1=degraded attempt2=degraded same-exit-ip | y" } as never), "probeNote.sameExitIP");
	strictEqual(probeFailureNote({ detail: "attempt1=error | z" } as never), "probeNote.attempt1Error");
	strictEqual(probeFailureNote({ detail: "attempt1=error | cause=transport | transport_error_not_evidence" } as never), "probeNote.transportError");
	strictEqual(probeFailureNote({ detail: "load account: 账号不存在 | w" } as never), "probeNote.accountMissing");
	strictEqual(probeFailureNote({ detail: "jury single-attempt" } as never), "");
});

test("色调映射:处置/裁决语义色稳定", () => {
	strictEqual(dispositionTone("remanded"), "destructive");
	strictEqual(dispositionTone("released"), "ok");
	strictEqual(verdictTone("account_guilty"), "destructive");
	strictEqual(verdictTone("exit_guilty"), "warning");
	strictEqual(verdictTone("dismissed"), "ok");
	deepStrictEqual(dispositionTone("withdrawn"), "muted");
});


test("探针汇总:时间戳不可解析的已中止任务仍计数(可见优先同口径)", () => {
	const now = Date.now();
	const tasks = [
		{ id: 1, case_id: 1, direction: "exit_jury", defendant: 1, juror: 2, state: "cancelled", result: "", detail: "", created_at: "not-a-date", finished_at: "not-a-date" },
	] as unknown as Array<Parameters<typeof probeSummary>[0][number]>;
	const summary = probeSummary(tasks, now);
	strictEqual(summary.cancelled, 1, "不可解析时间戳的中止任务必须可见计数");
});


test("探针汇总窗口可参数化:调宽窗口后滑出结论回归计数(批10 漂移修复锁定)", () => {
	const now = Date.now();
	const iso = (offsetMs: number) => new Date(now - offsetMs).toISOString();
	const tasks = [
		{ id: 1, case_id: 1, direction: "exit_jury", defendant: 1, juror: 2, state: "done", result: "degraded", detail: "", created_at: iso(45 * 60 * 1000), finished_at: iso(45 * 60 * 1000) },
	] as unknown as Array<Parameters<typeof probeSummary>[0][number]>;
	// 默认 30m 窗:已滑出不计。
	strictEqual(probeSummary(tasks, now).degraded, 0);
	// 调宽到 60m(管理员调整 evidence_window 后的形态):回归计数。
	strictEqual(probeSummary(tasks, now, 60 * 60 * 1000).degraded, 1);
});

// —— 裁决叙事:探针行计数与后端可采性同口径,叙事分支选择正确 ——

function probeTask(partial: Partial<QualityProbeTask>): QualityProbeTask {
	return {
		id: partial.id ?? 1, case_id: partial.case_id ?? 1,
		direction: partial.direction ?? "account_differential",
		defendant: partial.defendant ?? 7, node_id: partial.node_id ?? 2, epoch: partial.epoch ?? 0,
		juror: partial.juror ?? 0, state: partial.state ?? "done",
		result: partial.result ?? "clean", detail: partial.detail ?? "",
		created_at: partial.created_at ?? "2026-09-05T00:00:00Z",
		verified_ip_change: partial.verified_ip_change,
	};
}

test("tallyCaseProbes 只计可采结论(未验证 IP 的降智差分不是票)", () => {
	const tally = tallyCaseProbes({
		account: [
			probeTask({ id: 1, result: "clean" }),
			probeTask({ id: 2, result: "degraded", verified_ip_change: true }),
			probeTask({ id: 3, result: "degraded", verified_ip_change: false }),
			probeTask({ id: 4, state: "failed", result: "" }),
			probeTask({ id: 5, state: "cancelled", result: "" }),
			probeTask({ id: 6, state: "running", result: "" }),
		],
		exit: [
			probeTask({ id: 7, direction: "exit_jury", juror: 11, result: "clean" }),
			probeTask({ id: 8, direction: "exit_jury", juror: 12, result: "degraded" }),
			probeTask({ id: 9, direction: "exit_jury", juror: 13, state: "failed", result: "" }),
		],
	});
	deepStrictEqual(tally, {
		juryTotal: 2, juryClean: 1, juryDegraded: 1,
		diffValid: 2, diffClean: 1, diffDegraded: 1,
		pending: 1,
	});
});

test("verdictNarrative 按裁决与规则选择叙事分支并携带真实身份", () => {
	const base = {
		verdict: "account_guilty", status: "closed", rule: "account_differential_conviction",
		reason: "", defendantLabel: "acct-a", exitLabel: "node-1@3",
		juryNeedN: 4, diffNeed: 3, diffTransportFailed: null, thresholdPercent: 90,
	};
	const tally = { juryTotal: 4, juryClean: 4, juryDegraded: 0, diffValid: 3, diffClean: 0, diffDegraded: 3, pending: 0 };
	const differential = verdictNarrative({ ...base, tally });
	strictEqual(differential?.key, "quality.tribunal.narrative.accountDifferential");
	strictEqual(differential?.params.juryClean, 4);
	strictEqual(differential?.params.diffDegraded, 3);
	strictEqual(differential?.params.defendant, "acct-a");

	const transport = verdictNarrative({
		...base, rule: "account_transport_pattern_conviction",
		diffTransportFailed: 5, tally: { ...tally, diffValid: 0, diffClean: 0, diffDegraded: 0 },
	});
	strictEqual(transport?.key, "quality.tribunal.narrative.accountTransportPattern");
	strictEqual(transport?.params.transportFailed, 5);

	const exit = verdictNarrative({ ...base, verdict: "exit_guilty", tally });
	strictEqual(exit?.key, "quality.tribunal.narrative.exitGuilty");
	strictEqual(exit?.params.juryDegraded, 0);

	const timeout = verdictNarrative({
		...base, verdict: "insufficient", reason: "investigation_timeout_sso_unavailable",
		tally: { ...tally, diffValid: 1 },
	});
	strictEqual(timeout?.key, "quality.tribunal.narrative.insufficientTimeout");
	strictEqual(timeout?.params.threshold, 90);

	const plain = verdictNarrative({ ...base, verdict: "insufficient", rule: "", tally });
	strictEqual(plain?.key, "quality.tribunal.narrative.insufficient");
	strictEqual(plain?.params.juryNeedN, 4);

	// 探针行缺失(取数失败)不得编造叙事——回退通用文案由调用方处理。
	strictEqual(verdictNarrative({ ...base, tally: null }), null);
});

test("waitingReasonKey 只接受后端稳定码", () => {
	strictEqual(waitingReasonKey("deadline_reached"), "quality.tribunal.waitingReason.deadline_reached");
	strictEqual(waitingReasonKey("awaiting_probes"), "quality.tribunal.waitingReason.awaiting_probes");
	strictEqual(waitingReasonKey("probes_settled"), "quality.tribunal.waitingReason.probes_settled");
	strictEqual(waitingReasonKey("something_new"), null);
	strictEqual(waitingReasonKey(undefined), null);
});
