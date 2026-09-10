import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
(globalThis as Record<string, unknown>).window = { ...globalThis, location: { origin: "http://test.local" } };
const api = await import("./client.ts");
const session = await import("@/shared/auth/session");
const originalFetch = globalThis.fetch;
afterEach(() => { session.endSession(); globalThis.fetch = originalFetch; });
const passthrough = (value: unknown) => value;
const tokens = (accessToken: string) => ({ accessToken, accessTokenExpiresAt: "tomorrow", refreshTokenExpiresAt: "later" });
const unauthorized = () => Response.json({ error: { code: "adminUnauthorized", message: "expired" } }, { status: 401 });
function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(r => { resolve = r; }); return { promise, resolve }; }
const isAbort = (error: unknown) => error instanceof Error && error.name === "AbortError";
async function until(check: () => boolean) { for (let i = 0; i < 100 && !check(); i++)
    await new Promise(r => setTimeout(r, 2)); assert.ok(check()); }
for (const kind of ["json", "download", "stream"] as const) {
    test(`${kind} body is canceled on session end, and cannot publish old data`, async () => {
        let canceled = false;
        let requestSignal: AbortSignal | undefined;
        let body: ReadableStreamDefaultController<Uint8Array> | undefined;
        let received = false;
        const stream = new ReadableStream<Uint8Array>({ start(controller) { body = controller; }, cancel() { canceled = true; } });
        const response = new Response(stream, { status: 200, headers: { "Content-Type": kind === "stream" ? "text/event-stream" : "application/json" } });
        globalThis.fetch = async (_url, options) => { requestSignal = options?.signal ?? undefined; return response; };
        const request = kind === "json" ? api.apiRequest("/test", {}, () => { received = true; }) : kind === "download" ? api.apiDownloadResponse("/test").then(() => { received = true; }) : api.apiEventStream("/test", {}, passthrough, () => { received = true; });
        const result = request.catch(error => error);
        await until(() => Boolean(requestSignal));
        await new Promise(r => setTimeout(r, 2));
        session.endSession();
        assert.ok(requestSignal?.aborted);
        assert.ok(isAbort(await result));
        if (kind === "stream") {
            await until(() => canceled);
        }
        else {
            body?.enqueue(new TextEncoder().encode('{"data":{"stale":true}}'));
            body?.close();
        }
        await new Promise(r => setTimeout(r, 2));
        assert.equal(received, false, "old body must not reach a decoder, callback, or download consumer");
    });
}
test("concurrent 401 requests share refresh, retry once, and final 401 ends session", async () => {
    session.acceptSessionToken(session.currentSession(), "expired");
    let refreshes = 0;
    let requests = 0;
    const refresh = deferred<Response>();
    let failRetry = false;
    globalThis.fetch = async (url, options) => {
        if (String(url).endsWith("/refresh")) {
            refreshes++;
            return refresh.promise;
        }
        requests++;
        return new Headers(options?.headers).get("Authorization") === "Bearer renewed" && !failRetry ? Response.json({ data: "ok" }) : unauthorized();
    };
    const a = api.apiRequest("/one", {}, passthrough);
    const b = api.apiRequest("/two", {}, passthrough);
    await until(() => refreshes === 1);
    refresh.resolve(Response.json({ data: tokens("renewed") }));
    assert.deepEqual(await Promise.all([a, b]), ["ok", "ok"]);
    assert.equal(refreshes, 1);
    assert.equal(requests, 4);
    failRetry = true;
    const before = session.currentSession();
    await assert.rejects(api.apiRequest("/three", { retryAuth: false }, passthrough), isAbort);
    assert.equal(session.isCurrentSession(before), false);
});
test("caller cancellation is preserved and does not invalidate another active request", async () => {
    const pending = deferred<Response>();
    let count = 0;
    globalThis.fetch = async () => { count++; return pending.promise; };
    const caller = new AbortController();
    const before = session.currentSession();
    const first = api.apiRequest("/cancel", { signal: caller.signal }, passthrough).catch(error => error);
    const second = api.apiRequest("/keep", {}, passthrough);
    await until(() => count === 2);
    caller.abort();
    assert.ok(isAbort(await first));
    assert.ok(session.isCurrentSession(before));
    pending.resolve(Response.json({ data: "kept" }));
    assert.equal(await second, "kept");
});
test("real HTTP JSON, download, and progress sockets close when the session ends", async (t) => {
    const { createServer } = await import("node:http");
    for (const kind of ["json", "download", "stream"] as const) {
        await t.test(kind, async () => {
            const closed = deferred<void>();
            const started = deferred<void>();
            const server = createServer((_request, response) => {
                response.writeHead(200, { "Content-Type": kind === "stream" ? "text/event-stream" : "application/json" });
                response.write(kind === "stream" ? 'data: {"step":1}\n\n' : '{"data":');
                response.on("close", () => closed.resolve());
                started.resolve();
            });
            await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
            const address = server.address();
            assert.ok(address && typeof address !== "string");
            globalThis.fetch = (input, options) => originalFetch(new URL(String(input), `http://127.0.0.1:${address.port}`), options);
            try {
                const request = kind === "json" ? api.apiRequest("/slow", {}, passthrough) : kind === "download" ? api.apiDownload("/slow") : api.apiEventStream("/slow", {}, passthrough, () => { });
                const result = request.catch(error => error);
                await started.promise;
                await new Promise(r => setTimeout(r, 5));
                session.endSession();
                assert.ok(isAbort(await result));
                let timedOut = false;
                const timeout = setTimeout(() => { timedOut = true; closed.resolve(); }, 1000);
                await closed.promise;
                clearTimeout(timeout);
                assert.equal(timedOut, false, "server-side stream remained open");
            }
            finally {
                server.closeAllConnections();
                await new Promise<void>(resolve => server.close(() => resolve()));
            }
        });
    }
});
