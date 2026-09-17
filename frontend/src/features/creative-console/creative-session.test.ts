import assert from "node:assert/strict";
import { test } from "node:test";
import { createServer } from "node:http";
import { endSession } from "@/shared/auth/session";
import { createChatResponse, generateImage, synthesizeSpeech, transcribeSpeech } from "@/entities/creative-console/creative-console-api";
const isAbort = (error: unknown) => error instanceof Error && error.name === "AbortError";
test("workbench requests retain explicit client key and stop local transport on logout", async (t) => {
    const originalFetch = globalThis.fetch;
    t.after(() => { globalThis.fetch = originalFetch; endSession(); });
    const cases: Record<string, () => Promise<unknown>> = {
        image: () => generateImage({ apiKey: "explicit-client-key", model: "image", prompt: "test", count: 1, aspectRatio: "1:1", resolution: "1024" }),
        chat: () => createChatResponse({ apiKey: "explicit-client-key", model: "chat", messages: [], reasoningEffort: "none", webSearch: false, xSearch: false }),
        tts: () => synthesizeSpeech({ apiKey: "explicit-client-key", model: "tts", text: "test", voiceId: "voice", language: "en" }),
        stt: () => transcribeSpeech({ apiKey: "explicit-client-key", model: "stt", file: new File(["fixture"], "audio.wav"), language: "en" }),
    };
    for (const [name, run] of Object.entries(cases))
        await t.test(name, async () => {
            let received!: () => void;
            const started = new Promise<void>(resolve => { received = resolve; });
            let disconnected!: () => void;
            const closed = new Promise<void>(resolve => { disconnected = resolve; });
            let authorization: string | undefined;
            const server = createServer((request, response) => {
                authorization = request.headers.authorization;
                response.writeHead(200, { "Content-Type": name === "chat" ? "text/event-stream" : "application/json" });
                response.write(name === "chat" ? 'data: {"type":"response.output_text.delta","delta":"hello"}\n\n' : '{"data":');
                response.on("close", disconnected);
                received();
            });
            await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
            const address = server.address();
            assert.ok(address && typeof address !== "string");
            let requested = "";
            globalThis.fetch = (input, options) => {
                requested = String(input instanceof Request ? input.url : input);
                const url = new URL(requested, "http://127.0.0.1:3000");
                return originalFetch(new URL(`${url.pathname}${url.search}`, `http://127.0.0.1:${address.port}`), options);
            };
            try {
                const result = run().catch(error => error);
                await started;
                // Public-key calls resolve runtimeConfig.publicApiBaseUrl instead
                // of a relative /v1 path; the fixture server only sees the path.
                assert.ok(requested.startsWith("http://127.0.0.1:3000/v1/"), `public call must use the configured public base URL: ${requested}`);
                await new Promise(r => setTimeout(r, 5));
                endSession();
                assert.ok(isAbort(await result));
                assert.equal(authorization, "Bearer explicit-client-key");
                let timedOut = false;
                const timeout = setTimeout(() => { timedOut = true; disconnected(); }, 1000);
                await closed;
                clearTimeout(timeout);
                assert.equal(timedOut, false);
            }
            finally {
                server.closeAllConnections();
                await new Promise<void>(resolve => server.close(() => resolve()));
            }
        });
});
