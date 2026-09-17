import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { JSDOM } from "jsdom";

let dom: JSDOM;
let react: typeof import("react");
let reactDOM: typeof import("react-dom/client");
let query: typeof import("@tanstack/react-query");
let hooks: typeof import("./account-queries");
let api: typeof import("./account-api");
const originals = new Map<string, PropertyDescriptor | undefined>();
const originalFetch = globalThis.fetch;
before(async () => {
 dom = new JSDOM('<div id="root"></div>', { url: "http://127.0.0.1:3000" });
 for (const [name, value] of Object.entries({ window: dom.window, document: dom.window.document, navigator: dom.window.navigator, IS_REACT_ACT_ENVIRONMENT: true })) {
  originals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
  Object.defineProperty(globalThis, name, { value, configurable: true, writable: true });
 }
 react = await import("react");
 reactDOM = await import("react-dom/client");
 query = await import("@tanstack/react-query");
 hooks = await import("./account-queries");
 api = await import("./account-api");
});
after(() => {
 globalThis.fetch = originalFetch;
 dom.window.close();
 for (const [name, descriptor] of originals) {
  if (descriptor) Object.defineProperty(globalThis, name, descriptor);
  else Reflect.deleteProperty(globalThis, name);
 }
});
async function until(check: () => boolean) {
 for (let i = 0; i < 100 && !check(); i++) await react.act(async () => { await new Promise(resolve => setTimeout(resolve, 5)); });
 assert.ok(check(), "query did not settle");
}

test("deletion invalidation refreshes case names and restricted count without a page reload", async (t) => {
 const client = new query.QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
 let deleted = false;
 const calls: URL[] = [];
 globalThis.fetch = async (input) => {
  const url = new URL(String(input), "http://localhost"); calls.push(url);
  if (url.pathname.endsWith("/identities")) {
   assert.equal(url.searchParams.get("ids"), "7");
   return Response.json({ data: { items: deleted ? [] : [{ id: "7", name: "Synthetic defendant", email: "defendant@example.invalid", provider: "grok_build" }] } });
  }
  assert.equal(url.searchParams.get("quality"), "restricted");
  assert.equal(url.searchParams.get("pageSize"), "1");
  return Response.json({ data: { items: [], total: deleted ? 0 : 1, page: 1, pageSize: 1 } });
 };
 let names: string[] = []; let count: number | undefined;
 function Owner() {
  const directory = hooks.useAccountDirectory([7, 7, 0]);
  const restricted = hooks.useRestrictedAccountCount();
  names = (directory.data?.items ?? []).map(item => item.name);
  count = restricted.data?.total;
  return null;
 }
 const root = reactDOM.createRoot(dom.window.document.getElementById("root")!);
 t.after(async () => { await react.act(async () => root.unmount()); client.clear(); });
 await react.act(async () => root.render(react.createElement(query.QueryClientProvider, { client }, react.createElement(Owner))));
 await until(() => names.length === 1 && count === 1);
 deleted = true;
 await react.act(async () => { await client.invalidateQueries({ queryKey: ["accounts"] }); });
 await until(() => names.length === 0 && count === 0);
 assert.equal(calls.filter(url => url.pathname.endsWith("/identities")).length, 2);
 assert.equal(calls.length, 4);
});

test("identity batches are bounded and cancellation prevents the next batch", async () => {
 const controller = new AbortController();
 let calls = 0;
 globalThis.fetch = async (input) => {
  const ids = new URL(String(input), "http://localhost").searchParams.get("ids")!.split(",");
  calls++;
  assert.ok(ids.length <= 500);
  controller.abort();
  return Response.json({ data: { items: [] } });
 };
 await assert.rejects(api.getAccountIdentities(Array.from({ length: 501 }, (_, i) => String(i + 1)), controller.signal), { name: "AbortError" });
 assert.equal(calls, 1);
});
