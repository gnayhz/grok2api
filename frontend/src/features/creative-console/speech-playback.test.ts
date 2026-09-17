import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { JSDOM } from "jsdom";
import type { TTSResult } from "@/entities/creative-console/creative-console-api";

let dom: JSDOM;
let react: typeof import("react");
let reactDOM: typeof import("react-dom/client");
let SpeechPlayback: typeof import("./speech-playback.tsx").SpeechPlayback;
let synthesizeSpeech: typeof import("@/entities/creative-console/creative-console-api").synthesizeSpeech;
const originals = new Map<string, PropertyDescriptor | undefined>();
const request = { apiKey: "fixture-client", model: "tts", text: "hello", voiceId: "eve", language: "en" };

before(async () => {
  dom = new JSDOM('<div id="root"></div>', { url: "http://127.0.0.1:3000" });
  for (const [name, value] of Object.entries({ window: dom.window, document: dom.window.document, navigator: dom.window.navigator, HTMLElement: dom.window.HTMLElement, IS_REACT_ACT_ENVIRONMENT: true })) {
    originals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { value, configurable: true, writable: true });
  }
  react = await import("react");
  reactDOM = await import("react-dom/client");
  ({ SpeechPlayback } = await import("./speech-playback.tsx"));
  ({ synthesizeSpeech } = await import("@/entities/creative-console/creative-console-api"));
});

after(() => {
  dom.window.close();
  for (const [name, descriptor] of originals) {
    if (descriptor) Object.defineProperty(globalThis, name, descriptor);
    else Reflect.deleteProperty(globalThis, name);
  }
});

test("speech display owns binary URLs across StrictMode, replacements and result removal", async (t) => {
  const active = new Map<string, Blob>();
  let created = 0;
  t.mock.method(URL, "createObjectURL", (blob: Blob) => { const url = `blob:fixture-${++created}`; active.set(url, blob); return url; });
  t.mock.method(URL, "revokeObjectURL", (url: string) => { assert.ok(active.delete(url), "URL revoked without ownership"); });
  const pause = t.mock.method(dom.window.HTMLMediaElement.prototype, "pause", () => {});
  const load = t.mock.method(dom.window.HTMLMediaElement.prototype, "load", () => {});
  t.mock.method(globalThis, "fetch", async () => new Response(new Uint8Array([1, 2, 3]), { headers: { "content-type": "audio/wav" } }));
  const first = await synthesizeSpeech(request);
  assert.equal(active.size, 0, "API decoder must not acquire display resources");
  assert.ok(first.source instanceof Blob);
  assert.deepEqual(new Uint8Array(await first.source.arrayBuffer()), new Uint8Array([1, 2, 3]));
  const root = reactDOM.createRoot(dom.window.document.getElementById("root")!);
  t.after(async () => { await react.act(async () => root.unmount()); });
  async function display(result: TTSResult | null) {
    await react.act(async () => root.render(react.createElement(react.StrictMode, null, result ? react.createElement(SpeechPlayback, { result }) : null)));
  }
  await display(first);
  assert.equal(active.size, 1);
  assert.equal(created, 2, "StrictMode setup/cleanup/setup exercised");
  const firstURL = dom.window.document.querySelector("audio")!.src;
  const second = await synthesizeSpeech(request);
  await display(second);
  assert.equal(active.size, 1);
  assert.equal(active.has(firstURL), false);
  assert.equal(dom.window.document.querySelector("audio")!.src, dom.window.document.querySelector("a")!.href);
  const json = { source: "data:audio/wav;base64,AQID", contentType: "audio/wav", duration: 0.1 };
  await display(json);
  assert.equal(active.size, 0);
  assert.equal(dom.window.document.querySelector("audio")!.src, json.source);
  assert.match(dom.window.document.body.textContent!, /audio\/wav · 0.10s/);
  await display(second);
  assert.equal(active.size, 1);
  const removed = dom.window.document.querySelector("audio")!;
  await display(null); // Also covers VoicePanel clearing the result after STT.
  assert.equal(active.size, 0);
  assert.equal(removed.hasAttribute("src"), false);
  assert.equal(pause.mock.callCount(), load.mock.callCount());
  assert.ok(pause.mock.callCount() >= 5);
});

test("JSON and binary decoding without a mounted display never acquire object URLs", async (t) => {
  t.mock.method(URL, "createObjectURL", () => { throw new Error("No display owns this response"); });
  t.mock.method(globalThis, "fetch", async () => Response.json({ audio: "AQID", content_type: "audio/wav", duration: 1.25 }));
  assert.deepEqual(await synthesizeSpeech(request), { source: "data:audio/wav;base64,AQID", contentType: "audio/wav", duration: 1.25 });
  t.mock.method(globalThis, "fetch", async () => new Response(new Uint8Array([4, 5]), { headers: { "content-type": "audio/wav" } }));
  const result = await synthesizeSpeech(request);
  assert.ok(result.source instanceof Blob);
  assert.deepEqual(new Uint8Array(await result.source.arrayBuffer()), new Uint8Array([4, 5]));
});
