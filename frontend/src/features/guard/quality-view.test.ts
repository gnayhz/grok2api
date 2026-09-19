import { readFileSync } from "node:fs";
import { join } from "node:path";
import { ok, strictEqual } from "node:assert";
import { test } from "node:test";

import { probeDecoder } from "@/entities/guard/quality-api";
import { buildQualityExitIPIndex, parseGoDurationMs, probeDurationMs, probeSummary, qualityAccountDisplay, qualityExitDisplay } from "./quality-view.ts";

// Synthetic fixtures exercise the current proof records.
function fixture(name: string): unknown {
	return JSON.parse(readFileSync(join(import.meta.dirname, "__fixtures__", `${name}.json`), "utf8"));
}

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

test("探针汇总:在飞=待执行+执行中,近期结论守恒(合成夹具)", () => {
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
		{ id: 1, case_id: 1, direction: "case_proof", defendant: 1, state: "failed", result: "error", detail: "", created_at: iso(2 * 60 * 60 * 1000), finished_at: iso(2 * 60 * 60 * 1000) },
		{ id: 2, case_id: 1, direction: "case_proof", defendant: 1, state: "failed", result: "error", detail: "", created_at: iso(5 * 60 * 1000), finished_at: iso(5 * 60 * 1000) },
		{ id: 3, case_id: 1, direction: "case_proof", defendant: 1, state: "cancelled", result: "", detail: "案件已结,取证中止", created_at: iso(5 * 60 * 1000), finished_at: iso(5 * 60 * 1000) },
		{ id: 4, case_id: 1, direction: "case_proof", defendant: 1, state: "running", result: "", detail: "", created_at: iso(2 * 60 * 60 * 1000) },
	] as unknown as Array<Parameters<typeof probeSummary>[0][number]>;
	const summary = probeSummary(tasks, now);
	strictEqual(summary.error, 1); // 两小时前的失败已滑出窗口
	strictEqual(summary.cancelled, 1); // 中止独立计数,不算失败
	strictEqual(summary.inFlight, 1); // 僵尸在飞不受窗口美化,始终可见
});

test("探针耗时:已完成任务 created→finished 非负(合成夹具)", () => {
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

test("探针汇总:时间戳不可解析的已中止任务仍计数(可见优先同口径)", () => {
	const now = Date.now();
	const tasks = [
		{ id: 1, case_id: 1, direction: "case_proof", defendant: 1, state: "cancelled", result: "", detail: "", created_at: "not-a-date", finished_at: "not-a-date" },
	] as unknown as Array<Parameters<typeof probeSummary>[0][number]>;
	const summary = probeSummary(tasks, now);
	strictEqual(summary.cancelled, 1, "不可解析时间戳的中止任务必须可见计数");
});

test("探针汇总窗口可参数化:调宽窗口后滑出结论回归计数(批10 漂移修复锁定)", () => {
	const now = Date.now();
	const iso = (offsetMs: number) => new Date(now - offsetMs).toISOString();
	const tasks = [
		{ id: 1, case_id: 1, direction: "case_proof", defendant: 1, state: "done", result: "error", proof: { results: [{ outcome: "degraded" }] }, detail: "", created_at: iso(45 * 60 * 1000), finished_at: iso(45 * 60 * 1000) },
	] as unknown as Array<Parameters<typeof probeSummary>[0][number]>;
	// 默认 30m 窗:已滑出不计。
	strictEqual(probeSummary(tasks, now).degraded, 0);
	// 调宽到 60m(管理员调整 evidence_window 后的形态):回归计数。
	strictEqual(probeSummary(tasks, now, 60 * 60 * 1000).degraded, 1);
});
