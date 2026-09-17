import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { JSDOM } from "jsdom";
import type { QualityGuardForm } from "@/entities/guard/quality-guard-model";
import type { SettingsSnapshotDTO } from "@/entities/settings/settings-api";
import { settingsTestConfig } from "@/features/settings/settings-test-fixture.ts";

type SettingsHook = ReturnType<typeof import("@/features/settings/use-settings").useSettings>;
let dom: JSDOM;
let react: typeof import("react");
let query: typeof import("@tanstack/react-query");
let reactDOM: typeof import("react-dom/client");
let hookForm: typeof import("react-hook-form");
let useSettings: typeof import("@/features/settings/use-settings").useSettings;
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
  hookForm = await import("react-hook-form");
  ({ useSettings } = await import("@/features/settings/use-settings"));
});
after(() => {
  dom.window.close();
  for (const [name, descriptor] of originals) {
    if (descriptor) Object.defineProperty(globalThis, name, descriptor);
    else Reflect.deleteProperty(globalThis, name);
  }
});

function snapshot(revision: string, capacity: number, pending = false): SettingsSnapshotDTO {
  const config = settingsTestConfig();
  config.server.maxConcurrentRequests = capacity;
  return { config, revision, appliedRevision: pending ? (BigInt(revision)-1n).toString() : revision, applyPending: pending,
    applyTargets: [{ name: "network", appliedRevision: pending ? (BigInt(revision)-1n).toString() : revision, pending }],
    notification: { revision, state: "published" },
    restartRequired: [], updatedAt: "2026-09-08T00:00:00Z", recommendedProviderBuild: { clientVersion: "1.0.4", userAgent: "ua" } };
}
async function until(check: () => boolean, timeout = 1000) {
  const deadline = Date.now() + timeout;
  while (!check() && Date.now() < deadline) {
    await react.act(async () => { await new Promise((resolve) => setTimeout(resolve, 10)); });
  }
  assert.ok(check(), "condition did not settle");
}
async function mount() {
  const client = new query.QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false, gcTime: 0 } } });
  let current: SettingsHook | undefined;
  function Harness() {
    current = useSettings();
    return react.createElement("input", { type: "number", ...current.form.register("server.maxConcurrentRequests", { valueAsNumber: true }) });
  }
  const root = reactDOM.createRoot(dom.window.document.getElementById("root")!);
  await react.act(async () => { root.render(react.createElement(query.QueryClientProvider, { client }, react.createElement(Harness))); });
  const state = () => { assert.ok(current); return current; };
  await until(() => Boolean(state().settingsQuery.data));
  return { state, client, close: async () => { await react.act(async () => root.unmount()); client.clear(); } };
}

test("settings editor retains dirty values and CAS base across refresh, conflict, pending save and reconciliation", async (t) => {
  const oldFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = oldFetch; });
  let persisted = snapshot("9007199254740993", 2);
  const writes: Array<{ revision: string; config: SettingsSnapshotDTO["config"] }> = [];
  globalThis.fetch = async (_url, options) => {
    if (options?.method === "PUT") {
      const input = JSON.parse(options.body as string);
      writes.push(input);
      if (input.revision !== persisted.revision) return Response.json({ error: { code: "settingsConflict", message: "stale" } }, { status: 409 });
      persisted = { ...snapshot((BigInt(persisted.revision)+1n).toString(), input.config.server.maxConcurrentRequests, true), config: input.config };
      return Response.json({ data: persisted }, { status: 202 });
    }
    return Response.json({ data: persisted });
  };
  const view = await mount();
  t.after(view.close);
  const { state } = view;
  assert.equal(state().form.getValues().server.maxConcurrentRequests, 2);
  await react.act(async () => state().form.setValue("server.maxConcurrentRequests", 7, { shouldDirty: true }));
  persisted = snapshot("9007199254740994", 3, true);
  await react.act(async () => { await state().settingsQuery.refetch(); });
  await until(() => state().settingsQuery.data?.revision === persisted.revision);
  assert.equal(state().settingsQuery.data?.revision, "9007199254740994");
  assert.equal(state().settingsQuery.data?.applyPending, true);
  assert.equal(state().form.getValues().server.maxConcurrentRequests, 7);
  assert.equal(dom.window.document.querySelector("input")?.value, "7");
  await react.act(async () => { await assert.rejects(state().updateMutation.mutateAsync(state().form.getValues())); });
  assert.equal(writes[0].revision, "9007199254740993");
  assert.equal(state().form.getValues().server.maxConcurrentRequests, 7);
  assert.ok(state().form.formState.isDirty);
  await react.act(async () => state().reset());
  assert.equal(state().form.getValues().server.maxConcurrentRequests, 3);
  await react.act(async () => state().form.setValue("server.maxConcurrentRequests", 8, { shouldDirty: true }));
  await react.act(async () => { await state().updateMutation.mutateAsync(state().form.getValues()); });
  await until(() => state().settingsQuery.data?.revision === persisted.revision);
  assert.equal(writes[1].revision, "9007199254740994");
  assert.equal(state().settingsQuery.data?.applyPending, true);
  assert.equal(state().form.getValues().server.maxConcurrentRequests, 8);
  assert.equal(state().form.formState.isDirty, false);
  // Use the actual pending polling interval, then ensure a new dirty edit is
  // retained when the application status finishes in the background.
  await react.act(async () => state().form.setValue("server.maxConcurrentRequests", 9, { shouldDirty: true }));
  persisted = snapshot("9007199254740995", 8);
  await until(() => state().settingsQuery.data?.applyPending === false, 4500);
  assert.equal(state().form.getValues().server.maxConcurrentRequests, 9);
});

test("settings GET forwards cancellation and rejects malformed application metadata", async (t) => {
  const oldFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = oldFetch; });
  const { getSettings } = await import("@/entities/settings/settings-api");
  const controller = new AbortController();
  globalThis.fetch = async (_url, options) => {
    assert.equal(options?.signal, controller.signal);
    return new Promise<Response>((_resolve, reject) => options?.signal?.addEventListener("abort", () => reject(new DOMException("Aborted", "AbortError"))));
  };
  const request = getSettings(controller.signal);
  controller.abort();
  await assert.rejects(request);
  globalThis.fetch = async () => Response.json({ data: { ...snapshot("9007199254740993", 2), appliedRevision: 9007199254740992 } });
  await assert.rejects(getSettings(), /invalid|格式|无效/i);
  // Older servers remain readable without inventing an applied revision.
  const legacy = snapshot("9007199254740993", 2);
  delete legacy.appliedRevision; delete legacy.applyPending; delete legacy.applyTargets; delete legacy.notification;
  globalThis.fetch = async () => Response.json({ data: legacy });
  assert.equal((await getSettings()).appliedRevision, undefined);
});

test("guard and rotation editors save independent domains and freeze the guard edit revision", async (t) => {
  const oldFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = oldFetch; });
  const { useQualityGuardSettings } = await import("@/features/guard/use-quality-guard-settings");
  type GuardHook = ReturnType<typeof useQualityGuardSettings>;
  const baseline = { enabled: true, guarded_models: ["grok_build:grok-4.5", "custom-retired-model"], max_attempts: 2,
    created_timeout: "5s", evidence_timeout: "3.5s", admission_timeout: "30s", tool_admission_timeout: "3m",
    account_cooldown: "2m", idle_account_cooldown: "15m", reasoning_expected: true, exhaustion_policy: "fail_closed" };
  let guardSaved = { ...baseline, revision: "9007199254740993", file_defaults: baseline, self_check: { outcome: "ok" } };
  let gatewaySaved = snapshot("6", 2);
  const writes: Array<{ path: string; body: Record<string, unknown> }> = [];
  let failGuard = false;
  globalThis.fetch = async (url, options) => {
    const path = new URL(String(url), "http://127.0.0.1:3000").pathname;
    const isGuard = path === "/api/admin/v1/quality/guard";
    if (!options?.method || options.method === "GET") return Response.json({ data: isGuard ? guardSaved : gatewaySaved });
    const body = JSON.parse(options.body as string);
    writes.push({ path, body });
    if (isGuard) {
      if (failGuard) return Response.json({ error: { code: "quality_guard_update_failed", message: "storage unavailable" } }, { status: 500 });
      if (body.revision !== guardSaved.revision) return Response.json({ error: { code: "quality_guard_conflict", message: "conflict" } }, { status: 409 });
      guardSaved = { ...guardSaved, ...(options.method === "DELETE" ? baseline : body), revision: (BigInt(guardSaved.revision)+1n).toString() };
      return Response.json({ data: guardSaved });
    }
    assert.equal(body.revision, gatewaySaved.revision);
    gatewaySaved = { ...snapshot((BigInt(gatewaySaved.revision)+1n).toString(), 2), config: body.config ?? gatewaySaved.config };
    return Response.json({ data: gatewaySaved });
  };
  const client = new query.QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false, gcTime: 0 } } });
  let guardState: GuardHook | undefined;
  let gatewayState: SettingsHook | undefined;
  function Harness() {
    guardState = useQualityGuardSettings();
    gatewayState = useSettings();
    return react.createElement("div", null,
      react.createElement("input", { id: "guard-attempts", type: "number", ...guardState.form.register("maxAttempts", { valueAsNumber: true }) }),
      react.createElement(hookForm.Controller<QualityGuardForm, "createdTimeout">, {
        control: guardState.form.control, name: "createdTimeout",
        render: ({ field }) => react.createElement("input", {
          id: "controlled-duration", value: field.value?.value ?? "",
          onChange: (event: { target: { value: string } }) => field.onChange({ value: Number(event.target.value), unit: "s" }),
        }),
      }),
      react.createElement("input", { id: "capacity", type: "number", ...gatewayState.form.register("server.maxConcurrentRequests", { valueAsNumber: true }) }));
  }
  const root = reactDOM.createRoot(dom.window.document.getElementById("root")!);
  t.after(async () => { await react.act(async () => root.unmount()); client.clear(); });
  await react.act(async () => root.render(react.createElement(query.QueryClientProvider, { client }, react.createElement(Harness))));
  const guard = () => { assert.ok(guardState); return guardState; };
  const gateway = () => { assert.ok(gatewayState); return gatewayState; };
  await until(() => Boolean(guard().guardQuery.data && gateway().settingsQuery.data));
  assert.equal(guard().form.getValues().createdTimeout.value, 5);
  assert.equal(guard().form.formState.isDirty, false);
  await react.act(async () => {
    guard().form.setValue("maxAttempts", 17, { shouldDirty: true });
    guard().form.setValue("guardedModels", ["grok_build:grok-4.5", "custom-retired-model", "grok_web:grok-4"], { shouldDirty: true });
    gateway().form.setValue("server.maxConcurrentRequests", 12, { shouldDirty: true });
  });
  guardSaved = { ...guardSaved, revision: "9007199254740994", max_attempts: 3 };
  await react.act(async () => { await guard().guardQuery.refetch(); });
  await until(() => guard().guardQuery.data?.revision === "9007199254740994");
  assert.equal(guard().form.getValues().maxAttempts, 17);
  await react.act(async () => { await assert.rejects(guard().saveMutation.mutateAsync(guard().form.getValues())); });
  assert.equal(writes[0].path, "/api/admin/v1/quality/guard");
  assert.equal(writes[0].body.revision, "9007199254740993");
  assert.equal(writes.length, 1);
  assert.equal(gateway().form.getValues().server.maxConcurrentRequests, 12);
  await react.act(async () => guard().reload());
  await react.act(async () => guard().form.setValue("maxAttempts", 18, { shouldDirty: true }));
  failGuard = true;
  await react.act(async () => { await assert.rejects(guard().saveMutation.mutateAsync(guard().form.getValues())); });
  assert.equal(guard().form.getValues().maxAttempts, 18);
  assert.equal(gatewaySaved.revision, "6");
  failGuard = false;
  await react.act(async () => { await guard().saveMutation.mutateAsync(guard().form.getValues()); });
  await until(() => guard().guardQuery.data?.revision === "9007199254740995");
  assert.equal(writes.length, 3);
  assert.deepEqual(guardSaved.guarded_models, baseline.guarded_models);
  assert.equal(guardSaved.max_attempts, 18);
  assert.equal(gateway().form.getValues().server.maxConcurrentRequests, 12);
  await react.act(async () => { await gateway().updateMutation.mutateAsync(gateway().form.getValues()); });
  assert.equal(writes[3].path, "/api/admin/v1/settings");
  assert.equal("requestRetry" in (writes[3].body.config as object), false);
  assert.equal(guardSaved.revision, "9007199254740995");
  await react.act(async () => { await guard().resetMutation.mutateAsync(); });
  assert.equal(writes[4].path, "/api/admin/v1/quality/guard");
  assert.equal(guardSaved.max_attempts, baseline.max_attempts);
  assert.equal(gatewaySaved.config.server.maxConcurrentRequests, 12);
  await react.act(async () => { await gateway().resetRotationMutation.mutateAsync(); });
  assert.equal(writes[5].path, "/api/admin/v1/settings/egress-rotation/reset");
  assert.equal(guardSaved.revision, "9007199254740996");
});
