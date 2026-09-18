import assert from "node:assert/strict";
import { test } from "node:test";
import { settingsTestConfig } from "./settings-test-fixture.ts";
import { settingsSchema, toSettingsDTO, toSettingsForm } from "@/entities/settings/settings-form.ts";

test("session connection idle duration preserves units and validates its own limits", () => {
  const config = settingsTestConfig();
  config.providerBuild.sessionIdleConnTimeout = "300s";
  const form = toSettingsForm(config);
  assert.equal(settingsSchema.safeParse(form).success, true);
  form.providerBuild.sessionIdleConnTimeout = { value: 8, unit: "m" };
  assert.equal(toSettingsDTO(form).providerBuild.sessionIdleConnTimeout, "8m");
  for (const seconds of [0, 29, 1801]) {
    form.providerBuild.sessionIdleConnTimeout = { value: seconds, unit: "s" };
    const result = settingsSchema.safeParse(form);
    assert.equal(result.success, false);
    assert.ok(result.error?.issues.some((issue) => issue.path.join(".").startsWith("providerBuild.sessionIdleConnTimeout")));
  }
  for (const seconds of [30, 300, 1800]) {
    form.providerBuild.sessionIdleConnTimeout = { value: seconds, unit: "s" };
    assert.equal(settingsSchema.safeParse(form).success, true);
  }
});
