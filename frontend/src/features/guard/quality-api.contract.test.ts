import { readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";

import { caseDecoder, nodeDecoder, probeDecoder, settingsDecoder, qualitySettingsWrite } from "@/entities/guard/quality-api";

// 契约测试(批8 教训:端点验证≠面板验证):用 8003 真实响应夹具验证
// 前端解码器与后端包络/字段形状一致——解码失败=面板静默空列表的
// 事故永不重演。夹具更新:curl 三端点取 data 存 __fixtures__/。

function fixture(name: string): unknown {
	return JSON.parse(readFileSync(join(import.meta.dirname, "__fixtures__", `${name}.json`), "utf8"));
}

test("quality cases fixture decodes (羁押名单/裁决流数据流)", () => {
	const decoded = caseDecoder(fixture("cases"));
	if (decoded.items.length === 0) {
		throw new Error("cases fixture is empty — recapture from live deployment");
	}
	// 抓取时系统可能恰好健康(零在审)——live 承载在 cases-open
	// 夹具(真实案件还原为在审形态,形状=handler 契约)上断言。
	const open = caseDecoder(fixture("cases-open"));
	const withLive = open.items.filter((item) => item.live !== undefined);
	if (withLive.length === 0) {
		throw new Error("cases-open fixture lost its live views");
	}
});

test("quality probes fixture decodes (调查局队列数据流)", () => {
	const decoded = probeDecoder(fixture("probes"));
	if (decoded.items.length === 0) {
		throw new Error("probes fixture is empty — recapture from live deployment");
	}
});

test("quality egress fixture decodes (出口质量面数据流)", () => {
	const decoded = nodeDecoder(fixture("egress"));
	if (decoded.nodes.length === 0) {
		throw new Error("egress fixture is empty — recapture from live deployment");
	}
});

test("quality settings fixture decodes (仲裁庭参数数据流)", () => {
	// 设置页消费的参数面:直接闭环字段全解码(缺字段/类型漂移即红)。
	const decoded = settingsDecoder(fixture("settings-versioned"));
	if (!decoded.account_need_exits || !decoded.exit_need_n || !decoded.probe_budget) {
		throw new Error("settings fixture missing required fields");
	}
});

test("quality writes carry the observed version and exclude network capacity/application status", () => {
 const decoded = settingsDecoder(fixture("settings-versioned"));
 const write = qualitySettingsWrite(decoded);
 if (write.revision !== decoded.revision || "max_rotations_per_hour" in write || "applied" in write || "apply_pending" in write) {
  throw new Error("quality write changed its ownership or concurrency contract");
 }
});

test("quality pending apply retains separate durable and applied policy", () => {
 const original = settingsDecoder(fixture("settings-versioned"));
 const decoded = settingsDecoder({ ...original, revision: "2", apply_pending: true, apply_error: "reload failed", evidence_window: "2h0m0s" });
 if (!decoded.apply_pending || decoded.applied_revision !== "1" || decoded.applied.evidence_window === decoded.evidence_window) {
  throw new Error("pending application was presented as applied");
 }
});

test("quality version stays exact above JavaScript's safe integer range", () => {
 const original = settingsDecoder(fixture("settings-versioned"));
 const decoded = settingsDecoder({ ...original, revision: "9007199254740993" });
 if (qualitySettingsWrite(decoded).revision !== "9007199254740993") throw new Error("quality CAS clock rounded");
});
