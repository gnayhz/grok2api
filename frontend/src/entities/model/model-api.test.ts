import assert from "node:assert/strict";
import test from "node:test";
import { listModelAccountOptions, listModels } from "./model-api.ts";

test("model account options send the page and search and require an explicit total", async (t) => {
  const original = globalThis.fetch;
  t.after(() => { globalThis.fetch = original; });
  const controller = new AbortController();
  let transmittedSignal: AbortSignal | null | undefined;
  globalThis.fetch = async (url, options) => {
    const query = new URL(String(url), "http://127.0.0.1:3000").searchParams;
    assert.equal(query.get("provider"), "grok_build");
    assert.equal(query.get("page"), "21");
    assert.equal(query.get("pageSize"), "50");
    assert.equal(query.get("search"), "#9007199254740993");
    transmittedSignal = options?.signal;
    return Response.json({ data: { items: [{ id: "9007199254740993", name: "selected" }], page: 21, pageSize: 50, total: 1001 } });
  };
  const input = { provider: "grok_build" as const, page: 21, pageSize: 50, search: "#9007199254740993" };
  const page = await listModelAccountOptions(input, controller.signal);
  assert.equal(page.total, 1001);
  assert.equal(page.items[0].id, "9007199254740993");
  assert.ok(transmittedSignal);
  globalThis.fetch = async () => Response.json({ data: { items: [] } });
  await assert.rejects(listModelAccountOptions(input), { code: "invalidResponse" });
});

test("closing a model option query cancels its actual transport", async (t) => {
  const original = globalThis.fetch;
  t.after(() => { globalThis.fetch = original; });
  const controller = new AbortController();
  let canceled = false;
  let entered!: () => void;
  const started = new Promise<void>((resolve) => { entered = resolve; });
  globalThis.fetch = async (_url, options) => new Promise<Response>((_resolve, reject) => {
    assert.ok(options?.signal);
    options.signal.addEventListener("abort", () => { canceled = true; reject(options.signal?.reason); }, { once: true });
    entered();
  });
  const request = listModelAccountOptions({ provider: "grok_build", page: 1, pageSize: 50 }, controller.signal);
  const rejected = assert.rejects(request, { name: "AbortError" });
  await started;
  controller.abort();
  await rejected;
  assert.ok(canceled);
});


test("model capability compatibility field preserves old responses and rejects malformed facts", async (t) => {
 const original=globalThis.fetch;
 t.after(()=>{globalThis.fetch=original;});
 const route={id:"1",publicId:"legacy",provider:"grok_web",upstreamModel:"Web/grok-chat-fast",capability:"video",origin:"manual",enabled:true,accountIds:["1"],bindingMode:true,supportedAccounts:0,syncedAccounts:1,totalAccounts:1,capabilityKnown:true,available:false};
 for (const capabilitySupported of [undefined,false,true]) {
  globalThis.fetch=async ()=>Response.json({data:{items:[{...route,capabilitySupported}],page:1,pageSize:20,total:1}});
  const page=await listModels({page:1,pageSize:20});
  assert.equal(page.items[0].capabilitySupported,capabilitySupported);
  assert.equal(page.items[0].enabled,true);
 }
 globalThis.fetch=async ()=>Response.json({data:{items:[{...route,capabilitySupported:"false"}],page:1,pageSize:20,total:1}});
 await assert.rejects(listModels({page:1,pageSize:20}),{code:"invalidResponse"});
});
