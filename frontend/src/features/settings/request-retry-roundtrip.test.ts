import assert from "node:assert/strict";
import { test } from "node:test";
import { settingsTestConfig } from "./settings-test-fixture.ts";
import { settingsSchema, toSettingsDTO, toSettingsForm } from "./settings-model.ts";

test("gateway edits do not validate or write the guard read-only projection", () => {
  const config = settingsTestConfig();
  config.requestRetry = { enabled: true, maxAttempts: 100, onExhausted: "fail_closed", accountCooldown: "24h", evidenceTimeout: "24h", createdTimeout: "24h", idleAccountCooldown: "24h" };
  const form = toSettingsForm(config);
  assert.equal("requestRetry" in form, false);
  assert.equal(settingsSchema.safeParse(form).success, true);
  assert.equal("requestRetry" in toSettingsDTO(form), false);
});
