import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

(globalThis as Record<string, unknown>).window = { ...globalThis, location: { origin: "http://test.local" } };
const { listClientKeys, updateClientKey } = await import("./client-keys-api.ts");
const fixture = JSON.parse(readFileSync(new URL("./__fixtures__/reserved-budget.json", import.meta.url), "utf8"));

// The archived F03 response predates explicit scope; retain its budget values.
fixture.data.items[0].modelScope = "all";

test("client key API retains actual HTTP reservation separately from billed usage", async (t) => {
  const originalFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = originalFetch; });
  globalThis.fetch = async () => new Response(JSON.stringify(fixture), { status: 200 });
  const result = await listClientKeys({ page: 1, pageSize: 20 });
  assert.equal(result.items.length, 1);
  assert.equal(result.items[0].reservedUsageUsdTicks, 80000000);
  assert.equal(result.items[0].billedUsageUsdTicks, 0);
});

test("client key API accepts old servers and rejects malformed reservation values", async (t) => {
  const originalFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = originalFetch; });
  const legacy = structuredClone(fixture);
  delete legacy.data.items[0].reservedUsageUsdTicks;
  globalThis.fetch = async () => new Response(JSON.stringify(legacy), { status: 200 });
  const result = await listClientKeys({ page: 1, pageSize: 20 });
  assert.equal(result.items[0].reservedUsageUsdTicks, undefined);
  legacy.data.items[0].reservedUsageUsdTicks = "not a number";
  await assert.rejects(listClientKeys({ page: 1, pageSize: 20 }), (error: unknown) => error instanceof Error && "code" in error && error.code === "invalidResponse");
});


test("client key API keeps restricted-empty scope and sends only explicit patch fields", async (t) => {
  const originalFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = originalFetch; });
  const response = JSON.parse(readFileSync(new URL("./__fixtures__/model-scope.json", import.meta.url), "utf8"));
  const key = response.data.items[0];
  let sent: unknown;
  globalThis.fetch = async (_url, init) => {
    sent = JSON.parse(String(init?.body));
    return new Response(JSON.stringify({ ...fixture, data: key }), { status: 200 });
  };
  const result = await updateClientKey(key.id, { name: "renamed" });
  assert.deepEqual(sent, { name: "renamed" });
  assert.equal(result.modelScope, "restricted");
  assert.deepEqual(result.allowedModelIds, []);
  key.modelScope = "invalid";
  await assert.rejects(updateClientKey(key.id, { name: "renamed" }), (error: unknown) => error instanceof Error && "code" in error && error.code === "invalidResponse");
});
