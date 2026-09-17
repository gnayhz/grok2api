import assert from "node:assert/strict";
import { test } from "node:test";

import { i18n } from "@/shared/i18n";
import { ensureFeatureI18n, featureTranslationLoaders } from "./register-feature-i18n.ts";

test("feature copy is lazy, shared by concurrent navigation, bilingual and retryable", async (t) => {
  assert.equal(i18n.getResource("en", "translation", "creativeConsole"), undefined);
  assert.equal(i18n.getResource("en", "translation", "docs"), undefined);
  const original = featureTranslationLoaders.creativeConsole;
  let attempts = 0;
  const load = t.mock.method(featureTranslationLoaders, "creativeConsole", async () => {
    if (++attempts === 1) throw new Error("synthetic chunk failure");
    return original();
  });
  const failed = await Promise.allSettled([
    ensureFeatureI18n(["creativeConsole"]),
    ensureFeatureI18n(["creativeConsole"]),
  ]);
  assert.deepEqual(failed.map(result => result.status), ["rejected", "rejected"]);
  assert.equal(load.mock.callCount(), 1);
  await Promise.all([ensureFeatureI18n(["creativeConsole"]), ensureFeatureI18n(["creativeConsole"])]);
  await ensureFeatureI18n(["creativeConsole"]);
  assert.equal(load.mock.callCount(), 2);
  const expected = await original();
  for (const language of ["zh-CN", "en"] as const) {
    assert.deepEqual(i18n.getResource(language, "translation", "creativeConsole"), expected[language].creativeConsole);
    await i18n.changeLanguage(language);
    const copy = expected[language].creativeConsole;
    assert.notEqual(typeof copy, "string");
    assert.equal(i18n.t("creativeConsole.title"), (copy as { title: string }).title);
    assert.ok(i18n.exists("common.loading"));
  }
  assert.equal(i18n.getResource("en", "translation", "docs"), undefined);
});
