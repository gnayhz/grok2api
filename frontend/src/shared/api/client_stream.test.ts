import assert from "node:assert/strict";
import { test } from "node:test";

// client.ts 面向浏览器（window.setTimeout/navigator.locks）；node 测试
// 运行器下先垫最小全局再动态导入。
(globalThis as Record<string, unknown>).window = { ...globalThis, location: { origin: "http://test.local" } };
const { apiEventStream, ApiError } = await import("@/shared/api/client");

// apiEventStream 是账号导入/模型同步进度 UI 的 SSE 解析核心（分块边界/
// CRLF/多行 data/缓冲上限/内容类型守卫），此前零测试。

function sseResponse(chunks: string[], contentType = "text/event-stream", status = 200): Response {
  const encoder = new TextEncoder();
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(encoder.encode(chunk));
      controller.close();
    },
  });
  return new Response(stream, { status, headers: { "Content-Type": contentType } });
}

const passthrough = (value: unknown) => value;

test("dispatches events split across arbitrary chunk boundaries", async () => {
  const events: unknown[] = [];
  const body = JSON.stringify({ kind: "a" });
  const tail = JSON.stringify({ kind: "b" });
  const response = sseResponse(["data: " + body.slice(0, 8), body.slice(8) + "\n\n", "event: progress\ndata: " + tail + "\n\n"]);
  globalThis.fetch = (async () => response) as typeof fetch;
  await apiEventStream("/t", {}, passthrough, (e) => events.push(e.data));
  assert.equal(events.length, 2);
  assert.deepEqual(events[0], { kind: "a" });
  assert.deepEqual(events[1], { kind: "b" });
});

test("normalizes CRLF line endings and joins multi-line data", async () => {
  const events: { event: string; data: unknown }[] = [];
  const response = sseResponse(["event: step\r\ndata: [1,\r\ndata: 2]\r\n\r\n"]);
  globalThis.fetch = (async () => response) as typeof fetch;
  await apiEventStream("/t", {}, passthrough, (e) => events.push(e));
  assert.equal(events.length, 1);
  assert.equal(events[0].event, "step");
  assert.deepEqual(events[0].data, [1, 2]);
});

test("dispatches trailing block without terminator at stream end", async () => {
  const events: unknown[] = [];
  const response = sseResponse(["data: [\"tail\"]\n"]);
  globalThis.fetch = (async () => response) as typeof fetch;
  await apiEventStream("/t", {}, passthrough, (e) => events.push(e.data));
  assert.deepEqual(events, [["tail"]]);
});

test("rejects non-event-stream content type and cancels body", async () => {
  let cancelled = false;
  const stream = new ReadableStream<Uint8Array>({ start() {}, cancel() { cancelled = true; } });
  const response = new Response(stream, { status: 200, headers: { "Content-Type": "application/json" } });
  globalThis.fetch = (async () => response) as typeof fetch;
  await assert.rejects(
    apiEventStream("/t", {}, passthrough, () => {}),
    (err: unknown) => err instanceof ApiError && err.code === "invalidResponse",
  );
  assert.ok(cancelled, "body must be cancelled on content-type mismatch");
});

test("caps runaway blocks without terminator", async () => {
  const huge = "data: [" + "1".repeat(2 * 1024 * 1024) + "]";
  const response = sseResponse([huge]);
  globalThis.fetch = (async () => response) as typeof fetch;
  await assert.rejects(
    apiEventStream("/t", {}, passthrough, () => {}),
    (err: unknown) => err instanceof ApiError && err.code === "invalidResponse",
  );
});

test("rejects malformed JSON payloads", async () => {
  const response = sseResponse(["data: not-json\n\n"]);
  globalThis.fetch = (async () => response) as typeof fetch;
  await assert.rejects(
    apiEventStream("/t", {}, passthrough, () => {}),
    (err: unknown) => err instanceof ApiError && err.code === "invalidResponse",
  );
});
