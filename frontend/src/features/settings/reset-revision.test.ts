import assert from "node:assert/strict";
import { test } from "node:test";

import { resetSettings } from "./settings-api.ts";
import { ApiError } from "@/shared/api/client";

test("reset sends the exact observed revision and surfaces CAS conflict", async (t) => {
  const originalFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = originalFetch; });
  let sent: RequestInit | undefined;
  globalThis.fetch = async (_url, options) => {
    sent = options;
    return Response.json({ error: { code: "settingsConflict", message: "stale settings" } }, { status: 409 });
  };
  const revision = "9007199254740993";
  await assert.rejects(resetSettings(revision), (error: unknown) => {
    assert.ok(error instanceof ApiError);
    assert.equal(error.status, 409);
    assert.equal(error.code, "settingsConflict");
    return true;
  });
  assert.equal(sent?.method, "DELETE");
  assert.deepEqual(JSON.parse(sent?.body as string), { revision });
});
