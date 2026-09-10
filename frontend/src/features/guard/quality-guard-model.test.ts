import assert from "node:assert/strict";
import { test } from "node:test";
import { qualityGuardSchema, toQualityGuardForm, toQualityGuardInput } from "./quality-guard-model.ts";
import type { QualityGuardPolicy } from "./quality-api.ts";

const policy: QualityGuardPolicy = { enabled: true, guarded_models: ["grok_build:grok-4.5", "hidden-custom"], max_attempts: 100,
  created_timeout: "24h", evidence_timeout: "0s", admission_timeout: "30s", tool_admission_timeout: "3m",
  account_cooldown: "0s", idle_account_cooldown: "0s", reasoning_expected: true, exhaustion_policy: "fail_closed" };

test("guard form validates its own limits and preserves zeros, hidden model scope and exact revision", () => {
  const form = toQualityGuardForm(policy);
  assert.equal(qualityGuardSchema.safeParse(form).success, true);
  assert.equal(qualityGuardSchema.safeParse({ ...form, accountCooldown: { value: 168, unit: "h" } }).success, true);
  assert.equal(qualityGuardSchema.safeParse({ ...form, accountCooldown: { value: 169, unit: "h" } }).success, false);
  const input = toQualityGuardInput("9007199254740993", form);
  assert.equal(input.revision, "9007199254740993");
  assert.equal(input.account_cooldown, "0s");
  assert.equal(input.evidence_timeout, "0s");
  assert.equal(input.idle_account_cooldown, "0s");
  assert.equal(input.created_timeout, "24h");
  assert.deepEqual(input.guarded_models, policy.guarded_models);
  assert.equal("config" in input, false);
  for (const maxAttempts of [0, 101]) assert.equal(qualityGuardSchema.safeParse({ ...form, maxAttempts }).success, false);
  assert.equal(qualityGuardSchema.safeParse({ ...form, createdTimeout: { value: 25, unit: "h" } }).success, false);
  assert.equal(qualityGuardSchema.safeParse({ ...form, enabled: true, guardedModels: [] }).success, false);
  assert.equal(qualityGuardSchema.safeParse({ ...form, enabled: false, guardedModels: [] }).success, true);
});

test("guard API preserves decimal versions and reads only safe legacy numeric versions", async (t) => {
 const original = globalThis.fetch;
 t.after(() => { globalThis.fetch = original; });
 const { fetchQualityGuard } = await import("./quality-api.ts");
 for (const revision of ["9007199254740993", 5]) {
  globalThis.fetch = async () => Response.json({ data: { ...policy, revision, self_check: { outcome: "ok" } } });
  assert.equal((await fetchQualityGuard()).revision, String(revision));
 }
 globalThis.fetch = async () => Response.json({ data: { ...policy, revision: 9007199254740992, self_check: { outcome: "ok" } } });
 await assert.rejects(fetchQualityGuard());
});
