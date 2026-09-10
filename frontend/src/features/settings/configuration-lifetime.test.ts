import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { after, before, test } from "node:test";
import { JSDOM } from "jsdom";
import { settingsTestConfig } from "./settings-test-fixture.ts";
import type { SettingsSnapshotDTO } from "./settings-api.ts";

let dom: JSDOM;
let react: typeof import("react");
let reactDOM: typeof import("react-dom/client");
let query: typeof import("@tanstack/react-query");
let router: typeof import("react-router-dom");
let useSettings: typeof import("./use-settings.ts").useSettings;
let useGuard: typeof import("@/features/guard/use-quality-guard-settings").useQualityGuardSettings;
let QualityPage: typeof import("@/features/guard/quality-settings-page").QualitySettingsPage;
let TooltipProvider: typeof import("@/components/ui/tooltip").TooltipProvider;
const originals = new Map<string, PropertyDescriptor | undefined>();
const guardPolicy = { enabled: true, guarded_models: ["grok_build:grok-4.5"], max_attempts: 2, created_timeout: "5s", evidence_timeout: "3.5s", admission_timeout: "30s", tool_admission_timeout: "3m", account_cooldown: "2m", idle_account_cooldown: "15m", reasoning_expected: true, exhaustion_policy: "fail_closed" };
const tunables = JSON.parse(readFileSync(new URL("../guard/__fixtures__/settings-versioned.json", import.meta.url), "utf8"));
function gateway(revision: string): SettingsSnapshotDTO {
  return { config: settingsTestConfig(), revision, appliedRevision: revision, applyPending: false, applyTargets: [], notification: { revision, state: "published" }, restartRequired: [], updatedAt: "2026-09-10T00:00:00Z", recommendedProviderBuild: { clientVersion: "fixture", userAgent: "fixture" } };
}
function snapshot(domain: string, revision: string) {
  if (domain === "gateway") return gateway(revision);
  if (domain === "guard") return { ...guardPolicy, revision, file_defaults: guardPolicy, self_check: { outcome: "ok" } };
  return { ...tunables, revision, applied_revision: revision };
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((accept) => { resolve = accept; });
  return { promise, resolve };
}
async function until(check: () => boolean) {
  for (let i = 0; i < 100 && !check(); i++) await react.act(async () => { await new Promise((resolve) => setTimeout(resolve, 5)); });
  assert.ok(check(), "condition did not settle");
}
before(async () => {
  dom = new JSDOM('<div id="root"></div>', { url: "http://127.0.0.1:3000", pretendToBeVisual: true });
  originals.set("ResizeObserver", Object.getOwnPropertyDescriptor(globalThis, "ResizeObserver"));
  Object.defineProperty(globalThis, "ResizeObserver", { configurable: true, value: class { observe() {} unobserve() {} disconnect() {} } });
  for (const name of ["Node", "Element", "DocumentFragment", "MutationObserver", "CustomEvent", "Event"] as const) {
    originals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { value: dom.window[name], configurable: true, writable: true });
  }
  for (const [name, value] of Object.entries({ window: dom.window, document: dom.window.document, navigator: dom.window.navigator, HTMLElement: dom.window.HTMLElement, HTMLInputElement: dom.window.HTMLInputElement, HTMLFormElement: dom.window.HTMLFormElement, getComputedStyle: dom.window.getComputedStyle.bind(dom.window), requestAnimationFrame: dom.window.requestAnimationFrame.bind(dom.window), cancelAnimationFrame: dom.window.cancelAnimationFrame.bind(dom.window), IS_REACT_ACT_ENVIRONMENT: true })) {
    originals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { value, configurable: true, writable: true });
  }
  react = await import("react");
  reactDOM = await import("react-dom/client");
  query = await import("@tanstack/react-query");
  router = await import("react-router-dom");
  ({ useSettings } = await import("./use-settings.ts"));
  ({ useQualityGuardSettings: useGuard } = await import("@/features/guard/use-quality-guard-settings"));
  ({ QualitySettingsPage: QualityPage } = await import("@/features/guard/quality-settings-page"));
  ({ TooltipProvider } = await import("@/components/ui/tooltip"));
});
after(() => {
  dom.window.close();
  for (const [name, descriptor] of originals) {
    if (descriptor) Object.defineProperty(globalThis, name, descriptor);
    else Reflect.deleteProperty(globalThis, name);
  }
});

for (const [domain, actions] of [["gateway", ["save", "reset", "rotation"]], ["guard", ["save", "reset"]], ["tunables", ["save"]]] as const) {
  for (const action of actions) test(`${domain} ${action}: leaving the editor cancels transport and ignores a late acknowledgement`, async (t) => {
    const pending = deferred<Response>();
    const client = new query.QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false, gcTime: 0 } } });
    const key = domain === "gateway" ? ["settings"] : ["quality", domain === "guard" ? "guard" : "settings"];
    let savedRevision = "1";
    let writeSignal: AbortSignal | null | undefined;
    let writes = 0;
    t.mock.method(globalThis, "fetch", async (url: string | URL | Request, options?: RequestInit) => {
      const path = String(url);
      if (options?.method && options.method !== "GET") {
        writes++;
        writeSignal = options.signal;
        savedRevision = "2";
        return pending.promise; // Deliberately ignores cancellation, as a late transport can.
      }
      const target = path.endsWith("/quality/guard") ? "guard" : path.endsWith("/quality/settings") ? "tunables" : "gateway";
      return Response.json({ data: snapshot(target, savedRevision) });
    });
    let send: (() => Promise<unknown>) | undefined;
    let ready = false;
    function GatewayHarness() {
      const state = useSettings();
      ready = Boolean(state.settingsQuery.data);
      send = () => action === "save" ? state.updateMutation.mutateAsync(state.form.getValues()) : action === "reset" ? state.resetDefaultsMutation.mutateAsync() : state.resetRotationMutation.mutateAsync();
      return null;
    }
    function GuardHarness() {
      const state = useGuard();
      ready = Boolean(state.guardQuery.data);
      send = () => action === "save" ? state.saveMutation.mutateAsync(state.form.getValues()) : state.resetMutation.mutateAsync();
      return null;
    }
    let root = reactDOM.createRoot(dom.window.document.getElementById("root")!);
    let mounted = true;
    t.after(async () => { if (mounted) await react.act(async () => root.unmount()); client.clear(); });
    const content = domain === "tunables" ? react.createElement(router.MemoryRouter, { initialEntries: ["/guard/settings#tunables"] }, react.createElement(QualityPage)) : react.createElement(domain === "gateway" ? GatewayHarness : GuardHarness);
    const element = react.createElement(query.QueryClientProvider, { client }, react.createElement(TooltipProvider, null, content));
    await react.act(async () => root.render(element));
    let result: Promise<unknown> | undefined;
    if (domain === "tunables") {
      await until(() => Boolean(dom.window.document.getElementById("quality-tunable-retention")));
      const input = dom.window.document.getElementById("quality-tunable-retention")!;
      await react.act(async () => {
        Object.getOwnPropertyDescriptor(dom.window.HTMLInputElement.prototype, "value")!.set!.call(input, "169h");
        input.dispatchEvent(new dom.window.Event("input", { bubbles: true }));
      });
      const save = Array.from(input.closest("section")!.querySelectorAll("button")).find(button => button.textContent?.trim() === "保存");
      assert.ok(save);
      assert.equal(save.disabled, false);
      await react.act(async () => save.click());
    } else {
      await until(() => ready);
      await react.act(async () => { result = send!().catch(error => error); });
    }
    await until(() => writes === 1);
    await react.act(async () => root.unmount());
    mounted = false;
    savedRevision = "3";
    client.setQueryData(key, snapshot(domain, "3"));
    root = reactDOM.createRoot(dom.window.document.getElementById("root")!);
    await react.act(async () => root.render(element));
    mounted = true;
    await until(() => client.getQueryData<{ revision: string }>(key)?.revision === "3");
    const observed: string[] = [];
    const unsubscribe = client.getQueryCache().subscribe(() => {
      const revision = client.getQueryData<{ revision: string }>(key)?.revision;
      if (revision) observed.push(revision);
    });
    t.after(unsubscribe);
    pending.resolve(Response.json({ data: snapshot(domain, "2") }));
    await react.act(async () => { if (result) await result; await new Promise(resolve => setTimeout(resolve, 30)); });
    assert.equal(client.getQueryData<{ revision: string }>(key)?.revision, "3", "old editor overwrote the new page's accepted configuration");
    assert.equal(observed.includes("2"), false, "late acknowledgement briefly installed a stale version");
    assert.equal(writeSignal?.aborted, true, "page unmount did not cancel its write");
    assert.equal(writes, 1);
  });
}
