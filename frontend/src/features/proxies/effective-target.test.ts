import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { resolveEffectiveTarget, type EffectiveRoutingConfig } from "./effective-target.ts";

const node = (id: string) => ({ mode: "node" as const, nodeId: id });
const pool = (id: string) => ({ mode: "pool" as const, poolId: id });

const base: EffectiveRoutingConfig = {
  defaultTarget: { mode: "direct" },
  scopeTargets: { grok_build: node("7"), grok_web: { mode: "auto" } },
  classTargets: { inference: pool("3") },
};

// 与后端 TestOperationsConfigTargetFor 的用例逐条对齐:两份阶梯实现
// (前端 UI 预览 / 后端权威 resolveTarget)语义必须保持一致。
describe("resolveEffectiveTarget parity with backend TargetFor", () => {
  it("class rule wins over scope and default", () => {
    assert.deepEqual(resolveEffectiveTarget(base, "inference", "grok_build"), pool("3"));
  });

  it("scope rule wins over default for classes without a rule", () => {
    assert.deepEqual(resolveEffectiveTarget(base, "credential", "grok_build"), node("7"));
  });

  it("empty/unset class falls back to scope", () => {
    assert.deepEqual(resolveEffectiveTarget(base, undefined, "grok_build"), node("7"));
  });

  it("explicit auto scope beats the default", () => {
    assert.deepEqual(resolveEffectiveTarget(base, "inference", "grok_web"), pool("3"));
    assert.deepEqual(resolveEffectiveTarget(base, "credential", "grok_web"), { mode: "auto" });
  });

  it("unconfigured scope uses the default target", () => {
    assert.deepEqual(resolveEffectiveTarget(base, "inference", "grok_console"), pool("3"));
    assert.deepEqual(resolveEffectiveTarget(base, "billing", "grok_console"), { mode: "direct" });
  });

  it("auto default resolves to the automatic schedule", () => {
    const empty: EffectiveRoutingConfig = { defaultTarget: { mode: "auto" }, scopeTargets: {}, classTargets: {} };
    assert.deepEqual(resolveEffectiveTarget(empty, "inference", "grok_build"), { mode: "auto" });
  });

  it("explicit auto at every level resolves to automatic schedule", () => {
    const allAuto: EffectiveRoutingConfig = {
      defaultTarget: { mode: "auto" },
      scopeTargets: { grok_build: { mode: "auto" } },
      classTargets: { inference: { mode: "auto" } },
    };
    assert.deepEqual(resolveEffectiveTarget(allAuto, "inference", "grok_build"), { mode: "auto" });
  });
});
