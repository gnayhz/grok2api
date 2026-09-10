import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { JSDOM } from "jsdom";
import type { AuthContextValue } from "./auth-state.ts";
let dom: JSDOM;
let react: typeof import("react");
let query: typeof import("@tanstack/react-query");
let reactDOM: typeof import("react-dom/client");
let AuthProvider: typeof import("./auth-context.tsx").AuthProvider;
let SessionQueryProvider: typeof import("./session-query-provider.tsx").SessionQueryProvider;
let useAuth: typeof import("./use-auth.ts").useAuth;
let api: typeof import("@/shared/api/client");
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
    ({ SessionQueryProvider } = await import("./session-query-provider.tsx"));
    ({ AuthProvider } = await import("./auth-context.tsx"));
    ({ useAuth } = await import("./use-auth.ts"));
    api = await import("@/shared/api/client");
});
after(() => { dom.window.close(); for (const [name, descriptor] of originals) {
    if (descriptor)
        Object.defineProperty(globalThis, name, descriptor);
    else
        Reflect.deleteProperty(globalThis, name);
} });
const tokens = (accessToken: string) => ({ accessToken, accessTokenExpiresAt: "2026-09-09T00:00:00Z", refreshTokenExpiresAt: "2026-09-10T00:00:00Z" });
const loginResponse = (name: string) => Response.json({ data: { admin: { id: name, username: name }, tokens: tokens(name) } });
const unauthorized = () => Response.json({ error: { code: "adminUnauthorized", message: "expired" } }, { status: 401 });
function deferred<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(r => { resolve = r; }); return { promise, resolve }; }
async function until(check: () => boolean) { for (let i = 0; i < 100 && !check(); i++)
    await react.act(async () => { await new Promise(r => setTimeout(r, 5)); }); assert.ok(check(), "condition did not settle"); }
async function mount() {
    let client: InstanceType<typeof query.QueryClient>;
    let current: AuthContextValue | undefined;
    function Harness() { current = useAuth(); client = query.useQueryClient(); client.setDefaultOptions({ queries: { retry: false, gcTime: 0 }, mutations: { retry: false, gcTime: 0 } }); return react.createElement("div", null, current.status + ":" + (current.admin?.username ?? "")); }
    const root = reactDOM.createRoot(dom.window.document.getElementById("root")!);
    await react.act(async () => root.render(react.createElement(SessionQueryProvider, null, react.createElement(AuthProvider, null, react.createElement(Harness)))));
    const state = () => { assert.ok(current); return current; };
    return { state, get client() { return client; }, close: async () => { await react.act(async () => root.unmount()); } };
}
test("explicit logout clears actual query and mutation caches before re-login", async (t) => {
    const oldFetch = globalThis.fetch;
    t.after(() => { globalThis.fetch = oldFetch; });
    globalThis.fetch = async (url) => String(url).endsWith("/refresh") ? unauthorized() : String(url).endsWith("/login") ? loginResponse("first") : Response.json({ data: { loggedOut: true } });
    const view = await mount();
    t.after(view.close);
    await until(() => view.state().status === "anonymous");
    await react.act(async () => view.state().login("first", "password"));
    view.client.setQueryData(["accounts"], { secret: "old account data" });
    const mutation = view.client.getMutationCache().build(view.client, { mutationFn: async () => ({ secret: "exported" }) });
    await mutation.execute(undefined);
    await react.act(async () => view.state().logout());
    assert.equal(view.state().status, "anonymous");
    assert.equal(view.client.getQueryCache().getAll().length, 0, "logout retained managed query data");
    assert.equal(view.client.getMutationCache().getAll().length, 0);
    await react.act(async () => view.state().login("second", "password"));
    assert.equal(view.client.getQueryData(["accounts"]), undefined);
});
test("late refresh cannot replace token after logout and new login", async (t) => {
    const oldFetch = globalThis.fetch;
    t.after(() => { globalThis.fetch = oldFetch; });
    let refreshStarted = false;
    let refreshing = false;
    const stale = deferred<Response>();
    let authorization = "";
    globalThis.fetch = async (url, options) => {
        const path = String(url);
        if (path.endsWith("/refresh")) {
            if (!refreshing)
                return unauthorized();
            refreshStarted = true;
            return stale.promise;
        }
        if (path.endsWith("/login"))
            return loginResponse(JSON.parse(options?.body as string).username);
        if (path.endsWith("/logout"))
            return Response.json({ data: { loggedOut: true } });
        authorization = new Headers(options?.headers).get("Authorization") ?? "";
        return Response.json({ data: { ok: true } });
    };
    const view = await mount();
    t.after(view.close);
    await until(() => view.state().status === "anonymous");
    await react.act(async () => view.state().login("first", "password"));
    refreshing = true;
    const refresh = api.refreshAccessToken();
    await until(() => refreshStarted);
    await react.act(async () => view.state().logout());
    await react.act(async () => view.state().login("second", "password"));
    stale.resolve(Response.json({ data: tokens("stale-first") }));
    await react.act(async () => { await refresh; });
    await api.apiRequest("/api/admin/v1/accounts", {}, value => value);
    assert.equal(authorization, "Bearer second", "old refresh overwrote new session token");
    assert.equal(view.state().admin?.username, "second");
});
for (const failure of [false, true]) {
    test(`logout immediately isolates in-flight query and mutation; remote failure=${failure}`, async (t) => {
        const oldFetch = globalThis.fetch;
        t.after(() => { globalThis.fetch = oldFetch; });
        const pending = deferred<Response>();
        const logoutReply = deferred<Response>();
        const signals: AbortSignal[] = [];
        let started = 0;
        globalThis.fetch = async (url, options) => {
            const path = String(url);
            if (path.endsWith("/refresh"))
                return unauthorized();
            if (path.endsWith("/login"))
                return loginResponse(JSON.parse(options?.body as string).username);
            if (path.endsWith("/logout"))
                return logoutReply.promise;
            signals.push(options!.signal!);
            started++;
            return pending.promise;
        };
        const view = await mount();
        t.after(view.close);
        await until(() => view.state().status === "anonymous");
        await react.act(async () => view.state().login("first", "password"));
        const previous = view.client;
        previous.setQueryData(["accounts"], "old");
        const queryPromise = previous.fetchQuery({ queryKey: ["in-flight"], queryFn: () => api.apiRequest("/api/admin/v1/accounts", {}, value => value) }).catch(error => error);
        const mutation = previous.getMutationCache().build(previous, { mutationFn: () => api.apiRequest("/api/admin/v1/me/password", { method: "PUT" }, value => value), onError: () => { previous.setQueryData(["accounts"], "late rollback"); } });
        const mutationPromise = mutation.execute(undefined).catch(error => error);
        await until(() => started === 2);
        let logout!: Promise<unknown>;
        await react.act(async () => { logout = view.state().logout().catch(error => error); });
        assert.equal(view.state().status, "anonymous");
        assert.ok(signals.every(signal => signal.aborted));
        assert.notEqual(view.client, previous);
        assert.equal(view.client.getQueryData(["accounts"]), undefined);
        if (failure)
            logoutReply.resolve(Response.json({ error: { code: "requestFailed", message: "down" } }, { status: 503 }));
        else
            logoutReply.resolve(Response.json({ data: { loggedOut: true } }));
        await react.act(async () => { await logout; await queryPromise; await mutationPromise; });
        await react.act(async () => view.state().login("second", "password"));
        pending.resolve(Response.json({ data: "stale completed operation" }));
        await react.act(async () => { await new Promise(r => setTimeout(r, 5)); });
        assert.equal(view.state().admin?.username, "second");
        assert.equal(view.client.getQueryCache().getAll().length, 0);
        assert.equal(view.client.getMutationCache().getAll().length, 0);
    });
}
test("new login waits for pending remote logout without reviving local state", async (t) => {
    const oldFetch = globalThis.fetch;
    t.after(() => { globalThis.fetch = oldFetch; });
    const reply = deferred<Response>();
    let logins = 0;
    globalThis.fetch = async (url, options) => String(url).endsWith("/refresh") ? unauthorized() : String(url).endsWith("/logout") ? reply.promise : (logins++, loginResponse(JSON.parse(options?.body as string).username));
    const view = await mount();
    t.after(view.close);
    await until(() => view.state().status === "anonymous");
    await react.act(async () => view.state().login("first", "password"));
    let logout!: Promise<void>;
    let login!: Promise<void>;
    await react.act(async () => { logout = view.state().logout(); login = view.state().login("second", "password"); });
    assert.equal(logins, 1);
    assert.equal(view.state().status, "anonymous");
    reply.resolve(Response.json({ data: { loggedOut: true } }));
    await react.act(async () => { await logout; await login; });
    assert.equal(logins, 2);
    assert.equal(view.state().admin?.username, "second");
});
for (const phase of ["refresh", "me"] as const) {
    test(`late ${phase} 401 cannot end a later login`, async (t) => {
        const oldFetch = globalThis.fetch;
        t.after(() => { globalThis.fetch = oldFetch; });
        const reply = deferred<Response>();
        let started = false;
        globalThis.fetch = async (url, options) => {
            const path = String(url);
            if (path.endsWith("/login"))
                return loginResponse(JSON.parse(options?.body as string).username);
            if (path.endsWith("/refresh") && phase === "me")
                return Response.json({ data: tokens("old") });
            started = true;
            return reply.promise;
        };
        const view = await mount();
        t.after(view.close);
        await until(() => started);
        await react.act(async () => view.state().login("new", "password"));
        reply.resolve(unauthorized());
        await react.act(async () => { await new Promise(r => setTimeout(r, 5)); });
        assert.equal(view.state().status, "authenticated");
        assert.equal(view.state().admin?.username, "new");
    });
}
test("refresh unavailability is retryable; confirmed me 401 ends session and cache", async (t) => {
    const oldFetch = globalThis.fetch;
    t.after(() => { globalThis.fetch = oldFetch; });
    let available = false;
    let meOK = true;
    globalThis.fetch = async (url) => String(url).endsWith("/refresh") ? (available ? Response.json({ data: tokens("restored") }) : Response.json({ error: { code: "runtimeUnavailable", message: "offline" } }, { status: 503 })) : (meOK ? Response.json({ data: { id: "restored", username: "restored" } }) : unauthorized());
    const view = await mount();
    t.after(view.close);
    await until(() => view.state().status === "unavailable");
    available = true;
    await react.act(async () => view.state().retryRestore());
    assert.equal(view.state().admin?.username, "restored");
    view.client.setQueryData(["accounts"], "old");
    meOK = false;
    await react.act(async () => view.state().retryRestore());
    assert.equal(view.state().status, "anonymous");
    assert.equal(view.client.getQueryCache().getAll().length, 0);
});
test("a superseded pending login cannot set admin or its access token", async (t) => {
    const oldFetch = globalThis.fetch;
    t.after(() => { globalThis.fetch = oldFetch; });
    const reply = deferred<Response>();
    let firstStarted = false;
    let authorization = "";
    globalThis.fetch = async (url, options) => {
        if (String(url).endsWith("/refresh"))
            return unauthorized();
        if (String(url).endsWith("/login")) {
            const name = JSON.parse(options?.body as string).username;
            if (name === "first") {
                firstStarted = true;
                return reply.promise;
            }
            return loginResponse(name);
        }
        authorization = new Headers(options?.headers).get("Authorization") ?? "";
        return Response.json({ data: {} });
    };
    const view = await mount();
    t.after(view.close);
    await until(() => view.state().status === "anonymous");
    let first!: Promise<unknown>;
    await react.act(async () => { first = view.state().login("first", "password").catch(error => error); });
    await until(() => firstStarted);
    await react.act(async () => view.state().login("second", "password"));
    reply.resolve(loginResponse("first"));
    await react.act(async () => { await first; });
    await api.apiRequest("/api/admin/v1/accounts", {}, value => value);
    assert.equal(authorization, "Bearer second");
    assert.equal(view.state().admin?.username, "second");
});

test("remote logout timeout and fetch failure keep local session ended", async (t) => {
    const oldFetch = globalThis.fetch;
    t.after(() => { globalThis.fetch = oldFetch; });
    let hang = false;
    let logoutSignal: AbortSignal | null | undefined;
    globalThis.fetch = async (url, options) => {
        if (String(url).endsWith("/refresh")) return unauthorized();
        if (String(url).endsWith("/login")) return loginResponse("active");
        logoutSignal = options?.signal;
        if (!hang) throw new TypeError("Failed to fetch");
        return new Promise<Response>(() => {});
    };
    const view = await mount();
    t.after(view.close);
    await until(() => view.state().status === "anonymous");
    for (const neverResponds of [false, true]) {
        hang = neverResponds;
        await react.act(async () => view.state().login("active", "password"));
        const before = Date.now();
        let result!: Promise<unknown>;
        await react.act(async () => { result = view.state().logout().catch(error => error); });
        assert.equal(view.state().status, "anonymous");
        await react.act(async () => { assert.ok(await result instanceof Error); });
        assert.equal(view.state().status, "anonymous");
        assert.ok(Date.now() - before < 6_500, "local logout waited beyond its remote deadline");
        if (neverResponds) assert.ok(logoutSignal?.aborted);
    }
});
