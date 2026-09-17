import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { JSDOM } from "jsdom";

let dom: JSDOM;
let react: typeof import("react");
let reactDOM: typeof import("react-dom/client");
let query: typeof import("@tanstack/react-query");
let useLifetimeMutation: typeof import("./use-lifetime-mutation").useLifetimeMutation;
const originals = new Map<string, PropertyDescriptor | undefined>();
before(async () => {
  dom = new JSDOM('<div id="root"></div>', { url: "http://127.0.0.1:3000" });
  for (const [name, value] of Object.entries({ window: dom.window, document: dom.window.document, navigator: dom.window.navigator, IS_REACT_ACT_ENVIRONMENT: true })) {
    originals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { value, configurable: true, writable: true });
  }
  react = await import("react");
  reactDOM = await import("react-dom/client");
  query = await import("@tanstack/react-query");
  ({ useLifetimeMutation } = await import("./use-lifetime-mutation"));
});
after(() => {
  dom.window.close();
  for (const [name, descriptor] of originals) {
    if (descriptor) Object.defineProperty(globalThis, name, descriptor);
    else Reflect.deleteProperty(globalThis, name);
  }
});

test("changing owner cancels transport and suppresses old completion without affecting new work", async (t) => {
  const client = new query.QueryClient({ defaultOptions: { mutations: { retry: false, gcTime: 0 } } });
  const pending: Array<{ signal: AbortSignal; resolve: (value: number) => void }> = [];
  const notifications: string[] = [];
  let submit!: (value: number) => Promise<number>;
  function Owner({ name }: { name: string }) {
    const mutation = useLifetimeMutation({
      mutationFn: (_value: number, signal) => new Promise<number>(resolve => { pending.push({ signal, resolve }); }),
      onSuccess: value => { notifications.push(`${name}:success:${value}`); },
      onError: () => { notifications.push(`${name}:error`); },
      onSettled: () => { notifications.push(`${name}:settled`); },
    });
    submit = mutation.mutateAsync;
    return null;
  }
  const root = reactDOM.createRoot(dom.window.document.getElementById("root")!);
  t.after(async () => { await react.act(async () => root.unmount()); client.clear(); });
  const render = (name: string) => react.act(async () => root.render(react.createElement(query.QueryClientProvider, { client }, react.createElement(Owner, { key: name, name }))));
  await render("old");
  let old!: Promise<number | unknown>;
  await react.act(async () => { old = submit(1).catch(error => error); });
  await render("new");
  assert.ok(pending[0].signal.aborted);
  let current!: Promise<number>;
  await react.act(async () => { current = submit(2); });
  assert.equal(pending[1].signal.aborted, false);
  await react.act(async () => { pending[0].resolve(1); await old; });
  assert.deepEqual(notifications, []);
  await react.act(async () => { pending[1].resolve(2); await current; });
  assert.deepEqual(notifications, ["new:success:2", "new:settled"]);
});

test("StrictMode does not revive a submission queued by a discarded effect lifetime", async (t) => {
  const client = new query.QueryClient({ defaultOptions: { mutations: { retry: false, gcTime: 0 } } });
  let calls = 0;
  let successes = 0;
  let effectLifetimes = 0;
  function Owner() {
    const mutation = useLifetimeMutation({
      mutationFn: async (_value: void, signal) => { assert.equal(signal.aborted, false); calls++; return 1; },
      onSuccess: () => { successes++; },
    });
    const submit = react.useRef(mutation.mutate);
    react.useEffect(() => { effectLifetimes++; submit.current(); }, []);
    return null;
  }
  const root = reactDOM.createRoot(dom.window.document.getElementById("root")!);
  t.after(async () => { await react.act(async () => root.unmount()); client.clear(); });
  await react.act(async () => root.render(react.createElement(react.StrictMode, null, react.createElement(query.QueryClientProvider, { client }, react.createElement(Owner)))));
  assert.equal(effectLifetimes, 2);
  assert.equal(calls, 1);
  assert.equal(successes, 1);
});
