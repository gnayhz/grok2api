import assert from "node:assert/strict";
import { test } from "node:test";

// round 76：fetch 本身 reject（服务不可达/断网/DNS 失败）必须被归一化为
// 本地化的 networkError ApiError——此前浏览器原始 TypeError("Failed to
// fetch") 直接冒泡到 ErrorState，未本地化且语言混杂。

(globalThis as Record<string, unknown>).window = { ...globalThis, location: { origin: "http://test.local" } };
const { apiRequest, apiEventStream, ApiError } = await import("@/shared/api/client");

const passthrough = (value: unknown) => value;

test("apiRequest normalizes fetch rejection to localized networkError", async () => {
  globalThis.fetch = (async () => {
    throw new TypeError("Failed to fetch");
  }) as typeof fetch;
  await assert.rejects(
    apiRequest("/api/admin/v1/dashboard", {}, passthrough),
    (error: unknown) => {
      assert.ok(error instanceof ApiError);
      assert.equal(error.code, "networkError");
      assert.equal(error.status, 0);
      assert.ok(!/Failed to fetch/.test(error.message), "raw browser text must not leak");
      return true;
    },
  );
});

test("apiEventStream normalizes fetch rejection to localized networkError", async () => {
  globalThis.fetch = (async () => {
    throw new TypeError("NetworkError when attempting to fetch resource.");
  }) as typeof fetch;
  await assert.rejects(
    apiEventStream("/api/admin/v1/accounts/import", {}, passthrough, () => {}),
    (error: unknown) => {
      assert.ok(error instanceof ApiError);
      assert.equal(error.code, "networkError");
      return true;
    },
  );
});
