import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { after, before, test } from "node:test";
import { JSDOM } from "jsdom";
import type { AccountDTO } from "@/entities/account/account-api";

let dom: JSDOM;
let react: typeof import("react");
let reactDOM: typeof import("react-dom/client");
let query: typeof import("@tanstack/react-query");
let AccountEditor: typeof import("./account-editor").AccountEditor;
let useDeviceLogin: typeof import("./use-device-login").useDeviceLogin;
const originals = new Map<string, PropertyDescriptor | undefined>();
const originalFetch = globalThis.fetch;
before(async () => {
  dom = new JSDOM('<div id="root"></div>', { url: "http://127.0.0.1:3000", pretendToBeVisual: true });
  const values: Record<string, unknown> = { ResizeObserver: class { observe() {} unobserve() {} disconnect() {} }, window: dom.window, document: dom.window.document, navigator: dom.window.navigator, getComputedStyle: dom.window.getComputedStyle.bind(dom.window), IS_REACT_ACT_ENVIRONMENT: true };
  for (const name of ["Node", "NodeFilter", "Element", "HTMLElement", "HTMLInputElement", "HTMLFormElement", "DocumentFragment", "MutationObserver", "CustomEvent", "Event"] as const) values[name] = dom.window[name];
  for (const [name, value] of Object.entries(values)) {
    originals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { value, configurable: true, writable: true });
  }
  react = await import("react");
  reactDOM = await import("react-dom/client");
  query = await import("@tanstack/react-query");
  ({ AccountEditor } = await import("./account-editor"));
  ({ useDeviceLogin } = await import("./use-device-login"));
});
after(() => {
  globalThis.fetch = originalFetch;
  dom.window.close();
  for (const [name, descriptor] of originals) {
    if (descriptor) Object.defineProperty(globalThis, name, descriptor);
    else Reflect.deleteProperty(globalThis, name);
  }
});
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(accept => { resolve = accept; });
  return { promise, resolve };
}
async function until(check: () => boolean) {
  for (let i = 0; i < 100 && !check(); i++) await react.act(async () => { await new Promise(resolve => setTimeout(resolve, 5)); });
  assert.ok(check(), "condition did not settle");
}

test("a cancelled account save cannot close or reset the next editor", async (t) => {
  const fixture = JSON.parse(readFileSync(new URL("./__fixtures__/auth-diagnostic.json", import.meta.url), "utf8")).data as AccountDTO;
  const first = { ...fixture, id: "1", name: "Fictional account A" };
  const second = { ...fixture, id: "2", name: "Fictional account B" };
  const pending = deferred<Response>();
  let signal: AbortSignal | null | undefined;
  globalThis.fetch = async (url, options) => {
    assert.equal(options?.method, "PATCH");
    if (String(url).endsWith("/1")) { signal = options?.signal; return pending.promise; }
    assert.ok(String(url).endsWith("/2"));
    return Response.json({ data: second });
  };
  const closed: string[] = [];
  const client = new query.QueryClient({ defaultOptions: { mutations: { retry: false, gcTime: 0 } } });
  const root = reactDOM.createRoot(dom.window.document.getElementById("root")!);
  t.after(async () => { await react.act(async () => root.unmount()); client.clear(); globalThis.fetch = originalFetch; });
  const render = (account: AccountDTO) => react.act(async () => root.render(react.createElement(query.QueryClientProvider, { client }, react.createElement(AccountEditor, { key: account.id, account, onClose: () => closed.push(account.id) }))));
  const submit = () => react.act(async () => { dom.window.document.querySelector("form")!.dispatchEvent(new dom.window.Event("submit", { bubbles: true, cancelable: true })); });
  await render(first);
  await submit();
  await until(() => !!signal);
  await render(second);
  assert.ok(signal?.aborted);
  await react.act(async () => { pending.resolve(Response.json({ data: first })); await new Promise(resolve => setTimeout(resolve, 10)); });
  assert.deepEqual(closed, []);
  assert.equal(dom.window.document.querySelector<HTMLInputElement>("#account-name")!.value, second.name);
  await submit();
  await until(() => closed.length > 0);
  assert.deepEqual(closed, ["2"]);
});

test("closing and reopening device login discards late session creation and polling", async (t) => {
  const oldStart = deferred<Response>();
  const latePoll = deferred<Response>();
  const signals: AbortSignal[] = [];
  let starts = 0;
  let pollSignal: AbortSignal | null | undefined;
  let completed = 0;
  const makeSession = (id: string) => ({ sessionId: id, userCode: "FICTIONAL", verificationUri: "https://example.invalid/device", intervalSeconds: 0, expiresAt: "2099-01-01T00:00:00Z" });
  globalThis.fetch = async (url, options) => {
    if (String(url).endsWith("/device/start")) {
      signals.push(options!.signal!);
      return ++starts === 1 ? oldStart.promise : Response.json({ data: makeSession("fictional-new-session") });
    }
    assert.ok(String(url).includes("/fictional-new-session/poll"));
    pollSignal = options?.signal;
    return latePoll.promise;
  };
  let state!: ReturnType<typeof useDeviceLogin>;
  const onComplete = () => { completed++; };
  function Owner() { state = useDeviceLogin(onComplete); return null; }
  const root = reactDOM.createRoot(dom.window.document.getElementById("root")!);
  t.after(async () => { await react.act(async () => root.unmount()); globalThis.fetch = originalFetch; });
  await react.act(async () => root.render(react.createElement(Owner)));
  let old!: Promise<void>;
  await react.act(async () => { old = state.start(); });
  await until(() => starts === 1);
  await react.act(async () => state.close());
  assert.ok(signals[0].aborted);
  await react.act(async () => { await state.start(); });
  assert.equal(state.session?.sessionId, "fictional-new-session");
  await react.act(async () => { oldStart.resolve(Response.json({ data: makeSession("fictional-old-session") })); await old; });
  assert.equal(state.session?.sessionId, "fictional-new-session");
  await until(() => !!pollSignal);
  await react.act(async () => state.close());
  assert.ok(pollSignal?.aborted);
  await react.act(async () => { latePoll.resolve(Response.json({ data: { status: "succeeded" } })); await new Promise(resolve => setTimeout(resolve, 10)); });
  assert.equal(completed, 0);
  assert.equal(state.open, false);
});
