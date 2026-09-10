import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";

(globalThis as Record<string, unknown>).window = { ...globalThis, location: { origin: "http://test.local" } };
const { updateAccount } = await import("./accounts-api.ts");

// Emitted by the real GET handler in TestAccountAuthDiagnosticHTTPContract;
// single-account GET and PATCH responses use the same transport DTO.
function fixture() {
  return JSON.parse(readFileSync(join(import.meta.dirname, "__fixtures__", "auth-diagnostic.json"), "utf8"));
}

test("account decoder preserves independent authentication and health diagnostics", async () => {
  globalThis.fetch = async () => Response.json(fixture());
  const account = await updateAccount("1", { name: "auth-contract", priority: 1, maxConcurrent: 8, minimumRemaining: 0 });
  assert.equal(account.authStatus, "reauthRequired");
  assert.equal(account.authError, "Grok Web SSO credential rejected");
  assert.equal(account.lastError, "missing_thinking");
  assert.equal(account.failureCount, 3);
});

test("older responses may omit authError while present values require text", async () => {
  const payload = fixture();
  delete payload.data.authError;
  globalThis.fetch = async () => Response.json(payload);
  assert.equal((await updateAccount("1", { name: "auth-contract", priority: 1, maxConcurrent: 8, minimumRemaining: 0 })).authError, undefined);
  payload.data.authError = 42;
  await assert.rejects(updateAccount("1", { name: "auth-contract", priority: 1, maxConcurrent: 8, minimumRemaining: 0 }));
});
