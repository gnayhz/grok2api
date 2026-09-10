import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { JSDOM } from "jsdom";
import type { EgressOperationsValue } from "./operations-shared.ts";
import type { EgressOperationsConfigDTO } from "@/features/settings/settings-api";

let dom: JSDOM;
let react: typeof import("react");
let query: typeof import("@tanstack/react-query");
let reactDOM: typeof import("react-dom/client");
let Provider: typeof import("./operations-context.tsx").EgressOperationsProvider;
let useOperations: typeof import("./operations-shared.ts").useEgressOperations;
const originals = new Map<string, PropertyDescriptor | undefined>();

before(async () => {
  dom = new JSDOM('<div id="root"></div>', { url: "http://127.0.0.1:3000" });
  for (const [name, value] of Object.entries({ window: dom.window, document: dom.window.document, navigator: dom.window.navigator, HTMLElement: dom.window.HTMLElement, IS_REACT_ACT_ENVIRONMENT: true })) {
    originals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { value, configurable: true, writable: true });
  }
  react = await import("react");
  query = await import("@tanstack/react-query");
  reactDOM = await import("react-dom/client");
  ({ EgressOperationsProvider: Provider } = await import("./operations-context.tsx"));
  ({ useEgressOperations: useOperations } = await import("./operations-shared.ts"));
});

after(() => {
  dom.window.close();
  for (const [name, descriptor] of originals) {
    if (descriptor) Object.defineProperty(globalThis, name, descriptor);
    else Reflect.deleteProperty(globalThis, name);
  }
});

const initial: EgressOperationsConfigDTO = { probeProvider: "cloudflare", probeIntervalSeconds: 900, defaultTarget: { mode: "direct" }, scopeTargets: {}, classTargets: {}, updatedAt: "2026-09-10T00:00:00Z" };
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((accept) => { resolve = accept; });
  return { promise, resolve };
}
async function until(check: () => boolean) {
  for (let i = 0; i < 100 && !check(); i++) {
    await react.act(async () => { await new Promise((resolve) => setTimeout(resolve, 5)); });
  }
  assert.ok(check(), "condition did not settle");
}
async function mount(sharedClient?: InstanceType<typeof query.QueryClient>) {
  const client = sharedClient ?? new query.QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  let current: EgressOperationsValue | undefined;
  function Harness() {
    current = useOperations();
    return react.createElement("output", null, JSON.stringify(current.form));
  }
  const root = reactDOM.createRoot(dom.window.document.getElementById("root")!);
  await react.act(async () => root.render(react.createElement(query.QueryClientProvider, { client }, react.createElement(Provider, null, react.createElement(Harness)))));
  const state = () => { assert.ok(current); return current; };
  await until(() => !state().isPending);
  assert.equal(state().isError, false);
  return { state, client, close: async () => { await react.act(async () => root.unmount()); if (!sharedClient) client.clear(); } };
}

test("a completed network save retains newer edits and acknowledges only its submitted draft", async (t) => {
  const oldFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = oldFetch; });
  const pending = deferred<Response>();
  let stored = structuredClone(initial);
  const writes: EgressOperationsConfigDTO[] = [];
  globalThis.fetch = async (_url, options) => {
    if (options?.method === "PUT") {
      stored = { ...JSON.parse(options.body as string), updatedAt: initial.updatedAt };
      writes.push(structuredClone(stored));
      if (writes.length === 1) return pending.promise;
    }
    return Response.json({ data: stored });
  };
  const view = await mount();
  t.after(view.close);
  await react.act(async () => view.state().update((form) => ({ ...form, probeIntervalSeconds: 600 })));
  let saving!: Promise<boolean>;
  await react.act(async () => { saving = view.state().save(); });
  await until(() => writes.length === 1);
  assert.equal(view.state().savePending, true);
  await react.act(async () => view.state().update((form) => ({ ...form, probeIntervalSeconds: 1200 })));
  pending.resolve(Response.json({ data: stored }));
  await react.act(async () => { assert.equal(await saving, true); });
  assert.equal(writes[0].probeIntervalSeconds, 600);
  assert.equal(view.state().form.probeIntervalSeconds, 1200, "old acknowledgement cleared a newer draft");
  assert.equal(view.state().isDirty, true);
  assert.equal(view.client.getQueryData<EgressOperationsConfigDTO>(["egress-operations"])?.probeIntervalSeconds, 600);
  await react.act(async () => { assert.equal(await view.state().save(), true); });
  assert.equal(writes.length, 2);
  assert.equal(writes[1].probeIntervalSeconds, 1200);
  assert.equal(view.state().isDirty, false);
  assert.equal(view.state().form.probeIntervalSeconds, 1200);
});

test("network draft updates compose in the same render batch", async (t) => {
  const oldFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = oldFetch; });
  globalThis.fetch = async () => Response.json({ data: initial });
  const view = await mount();
  t.after(view.close);
  await react.act(async () => {
    view.state().update((form) => ({ ...form, probeIntervalSeconds: 600 }));
    view.state().update((form) => ({ ...form, probeProvider: "ipinfo" }));
  });
  assert.equal(view.state().form.probeIntervalSeconds, 600);
  assert.equal(view.state().form.probeProvider, "ipinfo");
  await react.act(async () => view.state().discard());
  assert.deepEqual(view.state().form, { probeProvider: initial.probeProvider, probeIntervalSeconds: initial.probeIntervalSeconds, defaultTarget: initial.defaultTarget, scopeTargets: {}, classTargets: {} });
});

test("saved network response replaces stale query data and cancels the earlier read", async (t) => {
  const oldFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = oldFetch; });
  const stale = deferred<Response>();
  let reads = 0;
  const readSignals: AbortSignal[] = [];
  globalThis.fetch = async (_url, options) => {
    if (options?.method === "PUT") return Response.json({ data: { ...initial, probeIntervalSeconds: 600 } });
    if (++reads > 1) {
      readSignals.push(options!.signal!);
      return stale.promise;
    }
    return Response.json({ data: initial });
  };
  const view = await mount();
  t.after(view.close);
  await react.act(async () => view.state().update((form) => ({ ...form, probeIntervalSeconds: 600 })));
  let reading!: Promise<void>;
  await react.act(async () => { reading = view.client.refetchQueries({ queryKey: ["egress-operations"], exact: true }); });
  await until(() => readSignals.length > 0);
  await react.act(async () => { assert.equal(await view.state().save(), true); });
  assert.equal(readSignals[0].aborted, true, "obsolete read kept its transport");
  assert.equal(view.state().isDirty, false);
  assert.equal(view.state().form.probeIntervalSeconds, 600, "saved response was discarded in favor of the old cache");
  stale.resolve(Response.json({ data: initial }));
  await react.act(async () => { await reading; });
  assert.equal(view.state().form.probeIntervalSeconds, 600);
});

test("failed network save keeps the draft available for retry", async (t) => {
  const oldFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = oldFetch; });
  let fail = true;
  globalThis.fetch = async (_url, options) => options?.method === "PUT"
    ? fail ? Response.json({ error: { code: "requestFailed", message: "fixture unavailable" } }, { status: 503 }) : Response.json({ data: { ...initial, ...JSON.parse(options.body as string) } })
    : Response.json({ data: initial });
  const view = await mount();
  t.after(view.close);
  await react.act(async () => view.state().update((form) => ({ ...form, probeIntervalSeconds: 600 })));
  await react.act(async () => { assert.equal(await view.state().save(), false); });
  assert.equal(view.state().form.probeIntervalSeconds, 600);
  assert.equal(view.state().isDirty, true);
  assert.equal(view.state().savePending, false);
  fail = false;
  await react.act(async () => { assert.equal(await view.state().save(), true); });
  assert.equal(view.state().form.probeIntervalSeconds, 600);
  assert.equal(view.state().isDirty, false);
});

test("unmounted network save cannot publish into the next page's shared query cache", async (t) => {
  const oldFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = oldFetch; });
  const pending = deferred<Response>();
  let stored = structuredClone(initial);
  let writes = 0;
  let oldSignal: AbortSignal | undefined;
  globalThis.fetch = async (_url, options) => {
    if (options?.method === "PUT") {
      stored = { ...initial, ...JSON.parse(options.body as string) };
      if (++writes === 1) {
        oldSignal = options.signal ?? undefined;
        return pending.promise;
      }
    }
    return Response.json({ data: stored });
  };
  const client = new query.QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const oldView = await mount(client);
  await react.act(async () => oldView.state().update((form) => ({ ...form, probeIntervalSeconds: 600 })));
  let saving!: Promise<boolean>;
  await react.act(async () => { saving = oldView.state().save(); });
  await until(() => Boolean(oldSignal));
  await oldView.close();
  assert.equal(oldSignal?.aborted, true);
  await react.act(async () => { assert.equal(await saving, false); });
  const view = await mount(client);
  t.after(async () => { await view.close(); client.clear(); });
  await react.act(async () => view.state().update((form) => ({ ...form, probeIntervalSeconds: 1200 })));
  await react.act(async () => { assert.equal(await view.state().save(), true); });
  pending.resolve(Response.json({ data: { ...initial, probeIntervalSeconds: 600 } }));
  await react.act(async () => { await new Promise((resolve) => setTimeout(resolve, 5)); });
  assert.equal(writes, 2);
  assert.equal(view.state().form.probeIntervalSeconds, 1200);
  assert.equal(view.client.getQueryData<EgressOperationsConfigDTO>(["egress-operations"])?.probeIntervalSeconds, 1200);
});
