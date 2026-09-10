import assert from "node:assert/strict";
import { test } from "node:test";
import { JSDOM } from "jsdom";

test("key editor preserves restricted-empty access and patches only changed fields", async (t) => {
  const dom = new JSDOM('<div id="root"></div>', { url: "http://test.local", pretendToBeVisual: true });
  let unmount = async () => {};
  const originals = new Map<string, PropertyDescriptor | undefined>();
  for (const [name, value] of Object.entries({
    window: dom.window, document: dom.window.document, navigator: dom.window.navigator,
    HTMLElement: dom.window.HTMLElement, HTMLInputElement: dom.window.HTMLInputElement,
    HTMLFormElement: dom.window.HTMLFormElement, HTMLSelectElement: dom.window.HTMLSelectElement,
    Element: dom.window.Element, Node: dom.window.Node, NodeFilter: dom.window.NodeFilter,
    Document: dom.window.Document, DocumentFragment: dom.window.DocumentFragment,
    HTMLButtonElement: dom.window.HTMLButtonElement,
    ResizeObserver: class { observe() {} unobserve() {} disconnect() {} },
    Event: dom.window.Event, CustomEvent: dom.window.CustomEvent, MutationObserver: dom.window.MutationObserver,
    getComputedStyle: dom.window.getComputedStyle, requestAnimationFrame: dom.window.requestAnimationFrame,
    cancelAnimationFrame: dom.window.cancelAnimationFrame, IS_REACT_ACT_ENVIRONMENT: true,
  })) {
    originals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { value, configurable: true, writable: true });
  }
  t.after(async () => {
    await unmount();
    dom.window.close();
    for (const [name, descriptor] of originals) {
      if (descriptor) Object.defineProperty(globalThis, name, descriptor);
      else Reflect.deleteProperty(globalThis, name);
    }
  });
  const react = await import("react");
  const { createRoot } = await import("react-dom/client");
  const { QueryClient, QueryClientProvider } = await import("@tanstack/react-query");
  const { i18n } = await import("@/shared/i18n");
  await i18n.changeLanguage("en");
  const { ClientKeysPage } = await import("./client-keys-page.tsx");
  const { TooltipProvider } = await import("@/components/ui/tooltip");
  const originalFetch = globalThis.fetch;
  t.after(() => { globalThis.fetch = originalFetch; });
  const key = { id: "17", name: "restricted key", prefix: "fixture", enabled: true,
    rpmLimit: 120, maxConcurrent: 8, billingLimitUsdTicks: 0, billedUsageUsdTicks: 0,
    allowModelAliases: false, modelScope: "restricted", allowedModelIds: [], providerScope: ["all"], tierScope: ["all"] };
  const writes: unknown[] = [];
  globalThis.fetch = async (url, options) => {
    if (options?.method === "PATCH") {
      const body = JSON.parse(options.body as string);
      writes.push(body);
      Object.assign(key, body);
      return Response.json({ data: key });
    }
    const items = String(url).includes("client-keys") ? [key] : [];
    return Response.json({ data: { items, total: items.length, page: 1, pageSize: 20 } });
  };
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false, gcTime: 0 } } });
  const root = createRoot(dom.window.document.getElementById("root")!);
  unmount = async () => { await react.act(async () => root.unmount()); client.clear(); };
  async function until(check: () => boolean) {
    const deadline = Date.now() + 3000;
    while (!check() && Date.now() < deadline) await react.act(async () => { await new Promise((resolve) => setTimeout(resolve, 10)); });
    assert.ok(check(), dom.window.document.body.textContent ?? "condition did not settle");
  }
  await react.act(async () => root.render(react.createElement(QueryClientProvider, { client }, react.createElement(TooltipProvider, null, react.createElement(ClientKeysPage)))));
  await until(() => Boolean(dom.window.document.body.textContent?.includes("Restricted · No allowed models")));
  async function openEditor() {
    await until(() => !dom.window.document.querySelector('[role="dialog"]'));
    const actions = dom.window.document.querySelector<HTMLButtonElement>('tbody button[aria-haspopup="menu"]');
    assert.ok(actions);
    await react.act(async () => {
      actions.dispatchEvent(new dom.window.MouseEvent("pointerdown", { bubbles: true, button: 0, ctrlKey: false }));
    });
    await until(() => Array.from(dom.window.document.querySelectorAll('[role="menuitem"]')).some((node) => node.textContent?.includes("Edit")));
    const edit = Array.from(dom.window.document.querySelectorAll<HTMLElement>('[role="menuitem"]')).find((node) => node.textContent?.includes("Edit"));
    assert.ok(edit);
    await react.act(async () => edit.click());
    await until(() => Boolean(dom.window.document.querySelector('[role="dialog"]')));
    return dom.window.document.querySelector('[role="dialog"]')!;
  }
  const dialog = await openEditor();
  assert.ok(dialog.textContent?.includes("This key cannot currently call any model."));
  const input = dialog.querySelector<HTMLInputElement>('input[name="name"]');
  assert.ok(input);
  await react.act(async () => {
    const setter = Object.getOwnPropertyDescriptor(dom.window.HTMLInputElement.prototype, "value")!.set!;
    setter.call(input, "renamed key");
    input.dispatchEvent(new dom.window.Event("input", { bubbles: true }));
  });
  const form = dialog.querySelector("form");
  assert.ok(form);
  await react.act(async () => form.dispatchEvent(new dom.window.Event("submit", { bubbles: true, cancelable: true })));
  await until(() => writes.length === 1);
  assert.deepEqual(writes, [{ name: "renamed key" }]);
  assert.equal(key.modelScope, "restricted");
  assert.deepEqual(key.allowedModelIds, []);
  const secondDialog = await openEditor();
  const enabled = secondDialog.querySelector<HTMLButtonElement>("#key-enabled");
  assert.ok(enabled);
  await react.act(async () => enabled.click());
  assert.equal(enabled.getAttribute("aria-checked"), "false");
  const secondForm = secondDialog.querySelector("form");
  assert.ok(secondForm);
  await react.act(async () => secondForm.dispatchEvent(new dom.window.Event("submit", { bubbles: true, cancelable: true })));
  await until(() => writes.length === 2);
  assert.deepEqual(writes, [{ name: "renamed key" }, { enabled: false }]);
  assert.equal(key.modelScope, "restricted");
  assert.deepEqual(key.allowedModelIds, []);

});
