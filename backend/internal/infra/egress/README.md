# Egress runtime

This document describes runtime ownership and invariants. For cross-module boundaries and extension work, start with the [architecture](../../../../ARCHITECTURE.md) and [development guide](../../../../DEVELOPMENT.md).

`Manager` is the compatibility facade for external callers (providers and the composition root); internal runtime code holds `*routingRuntime` and its components directly, so new forwarding wrappers must not be added to Manager. The request path resolves routing, applies authoritative quality admission, prepares the browser session if needed, and acquires a client handle. Providers own business retries and response parsing; the runtime owns connection resources, cancellation and completion observations.

## State ownership

| Component | Owned state and work |
| --- | --- |
| `routingRuntime` | Immutable node/operations/pool projections, routing generations, fixed-target cache, selection accounting, session pins and rotation cursors |
| `clientRegistry` | Client identities, cache generations, construction, live handles and retirement |
| `clearanceRuntime` | Solver configuration, per-binding cookie generations, shared solves and publication checks |
| `healthRuntime` | Four observation workers, bounded coalescing queues and immediate local health overlays |
| `taskRuntime` | Admission and lifecycle of shared loads, probes, solver tasks, cursor writes and idle reclamation |
| `netbudget.Runtime` | Socket, establishment, request, client and waiter permits; actual socket shutdown |
| `browsertransport.Transport` | Browser profile, origin initialization and HTTP/2 connection scheduling around fhttp/uTLS |

Component state is held through named fields. Cross-component work uses callbacks or methods; no component embeds a second Manager or shares ownership of another component's mutex.

Routing uses a dedicated SQL projection. It loads no source display joins, pool membership lists or rotation credentials. Node snapshots sort once on publication; requests filter immutable snapshots using reusable scratch storage. Pool accounting uses bounded LRU tables with constant-time updates for existing entries.

## Request and stream lifetime

A lease pins its client handle until `Release`; releases are idempotent. Each physical HTTP request additionally holds a request slot through body EOF, error, close or cancellation. WebSocket ownership transfers from handshake to the returned connection. Pinned HTTPS downloads use transient handles with the same client, request and socket budgets; their client permit remains owned through body completion or cancellation. Providers must still close their response bodies/connections and release leases; cancellation also releases runtime ownership.

The built-in Build fallback uses a managed environment-proxy client even when no route or account isolation is configured. It preserves `HTTP_PROXY`/`HTTPS_PROXY` while sharing the runtime budget and shutdown. Custom transports explicitly injected by embedders retain their own ownership. Canceling the lease's acquisition context releases its handle, including when the caller abandons an unread body.

Clearance RPCs, rotation webhooks and subscription fetches also use owned transport handles and the same global budgets. These control requests retain their existing proxy, destination and redirect rules. In particular, subscription downloads keep public-address validation and use the shared cancelable SOCKS dialer. They do not generate provider attempt or health observations.

Retiring a cached client closes its idle connections. Its active requests retain their handle and client permit, then perform final idle cleanup on completion. A retired handle is therefore counted even after it disappears from the cache. New client admission reclaims an idle entry in the same shared/session class, or reports capacity exhaustion if every eligible handle remains in use.

Browser HTTP/2 initialization is scoped to the origin and never runs while holding a shared state lock. Waiting requests can cancel independently. The connection pool avoids fhttp's connection mutex during selection and idle close. Socket writes have a ten-second limit; cancellation interrupts a write that remains blocked across a short grace window. Healthy multiplexed streams survive normal cancellation of another stream. Request retries retain fhttp's protocol-safe rules, and application retries never replay a request after submission may have begun.

TCP, proxy negotiation, TLS and HTTP/2 preface establishment have a ten-second budget. The establishment quota remains held until `GotConn`/WebSocket handoff; the deadline is then removed. Normal long response streams are governed by the caller and the existing provider stream-idle policy, not by the establishment deadline.

## Health and binding consistency

Production providers call `Lease.Observe`, which submits without waiting for storage. Errors obtained directly from physical transport calls/body reads are marked with their phase. Parsing errors, model errors, downstream writes, cancellation, stream-idle termination and Build response-header timeouts do not penalize a node. WebSocket callers mark direct upstream read/write errors explicitly.

Observations capture the encrypted proxy binding, `binding_revision` and `health_revision`. SQL updates increment failure counts atomically; success uses a revision predicate so an older success cannot erase a newer failure. Quality quarantine is preserved. Failures retain the latest of the existing cooldown, transport backoff and explicit minimum deadline; anti-bot rejection cannot clear a transport cooldown. Coalescing preserves transport failures and their timestamps even when a later observation is a 403. A local bounded overlay changes scheduling while persistence is pending, and cannot mask a later stored cooldown with a predicted revision tie. Workers coalesce bursts by binding, with 512 entries per shard and a two-second deadline per write. Queue overflow and storage errors are visible in runtime statistics. This is a bounded operational health signal, not a durable audit log: exhausted queues or an expired shutdown deadline can discard observations. `FlushFeedback` waits for accepted work to finish its persistence attempts; it is intended for maintenance and tests, never request delivery.

Node configuration edits advance binding generations. A request whose selection predates an edit is rejected before lease publication and reselects within a bounded retry count. Clearance writes compare the binding revision and prior clearance revision; configuration changes reject old solver completion. Each cached solve also has a generation, so an old lease's 403 cannot invalidate a newer cookie. Probe results remain ordered by their database sequence, and a healthy probe only clears transport failures present when that probe began.

Rotation attempt reservations and completion writes also compare the original binding revision, proxy and webhook. Editing a node during a webhook prevents its older bookkeeping result from updating the new configuration. A webhook already submitted before the edit cannot be recalled.

The application publishes rotation success and invokes its observation callback only after the completion write succeeds. A rejected completion keeps the reserved attempt and reports a state-write failure. Manual and dead-exit budget resets also carry the binding they inspected; a failed reset cannot enqueue a new rotation. Rotation bookkeeping requires the conditional repository operation and has no unversioned fallback. Historical unversioned writes exist only in migration test fixtures. Queue rejection is reported and is not counted as successful recovery.

The legacy `Feedback`/`FeedbackForScope` methods remain for embedders, but cannot carry an original lease binding. New integrations must use lease observations. Classified lease rejections invalidate Clearance synchronously by its captured key/generation; health persistence never repeats that invalidation. Cookie, User-Agent and generation travel together through cache hits, stale fallback and shared solves, so a concurrent client construction cannot attach a newer generation to an older cookie. Repeated invalidation of the same generation is idempotent.

## Resource policy and operations

Startup configuration lives in `egress.runtime` in the root configuration file:

```yaml
egress:
  runtime:
    maxConnections: 512
    maxDialing: 32
    maxRequests: 2048
    maxWaiters: 512
    maxClients: 2048
    queueTimeout: 2s
```

Zero uses the default. Limits apply per Manager/process and are not live routing settings. Connection accounting includes establishing, active, idle and retired sockets. A waiter may leave on caller cancellation, drain, queue timeout or capacity exhaustion. Capacity errors use `netbudget.ErrCapacity`; drain/close uses `netbudget.ErrClosed`. They are distinguishable from proxy failures and do not cool nodes. Browser origin/draining capacity uses the same capacity sentinel, and a caller deadline spent waiting for admission preserves both the deadline and capacity causes. Local admission errors also bypass proxy-pool connection retries.

IPv4/IPv6 probes use transient client handles with the same client, request and connection budgets as provider traffic. `GotConn` changes socket accounting from establishing to active; completion, cancellation and shutdown release the owner. Local admission, shutdown, cancellation and probe setup failures return `domain.ProbeExecutionError` with an unknown result, without overwriting the last completed database observation or feeding dead-exit confirmation. The administration API returns 503 for an incomplete probe. Batch operations return an operation error instead of counting such attempts as unreachable nodes; successful completed measurements in that batch still persist. If one address family succeeds while the other cannot execute locally, the successful family establishes reachability and the other remains unknown.

Build session pinning and environment proxy fallback are independent options: HTTP_PROXY, HTTPS_PROXY and NO_PROXY apply with or without sessions/account isolation. Explicit HTTP/SOCKS/tunnel configuration takes precedence over the environment policy.

Additional internal caps: 64 distinct shared loads per group, 64 active storage loads/client constructions, eight solves/probes/cursor writers per task category, 8,192 session pins, 16,384 clearance entries, and 4,096 fixed-target/pool/rotation cache entries. Shared loads use one broadcast channel per key, so canceled waiters leave no retained per-waiter channel. Delayed confirmation probes have 32 task slots and 4,096 retained observations; rotation has 4,096 queued nodes and 1,024 timers. Timer callbacks check generation and are canceled on shutdown.

Routing retains at most 4,096 inactive node counters in addition to active leases. Capacity cleanup also runs for fixed/pool-only workloads, without depending on a future full node snapshot refresh.

`GET /api/admin/v1/egress-operations/runtime` requires the existing administrator authentication. It exposes actual connection categories, active requests, clients, retired handles, waiters, task counts and dropped/error counters. Pool/routing statistics retain their existing endpoints.

`Start` enables periodic idle reclamation. `BeginDrain` rejects new work and cancels maintenance. `Drain(ctx)` waits for active requests/tasks and observation attempts. `Close(ctx)` closes remaining sockets and joins owned tasks and health writers before storage closes. The application stops HTTP admission, joins its maintenance loops and closes the egress runtime before the database. Subscription sync and scheduled probes run independently. Repeated close is supported.

## Verification

From the repository root, run `scripts/verify-egress-runtime.sh`. It runs the full backend suite, race checks for the affected packages, real local protocol/fault-isolation tests and sustained resource checks. The sustained normal/overload/recovery/hot-update scenario runs explicitly with the `egress_final_review` tag. Set `EGRESS_BENCH=1` to include microbenchmarks. PostgreSQL tests run when `TEST_POSTGRES_DSN` or the existing `TEST_POSTGRES_ADMIN_DSN` temporary-database harness is configured.

Key tests cover:

- Healthy response delivery while feedback storage blocks; cross-origin TLS isolation; cache eviction during a blocked browser handshake.
- Real HTTP/1.1 and HTTP/2 TLS, gzip, trailers, reuse, Chrome ClientHello/ALPN, and on-wire HTTP/2 SETTINGS/pseudo-header order.
- HTTP CONNECT buffering, SOCKS4a fragmented replies, SOCKS5 tunnels, cancellation of HTTP/HTTPS/SOCKS negotiation, tunnel initial-write cancellation and WebSocket binary transport.
- HTTP/2 blocked writes, reconnect waiters, unread-body cancellation, long streams and isolation of normal stream cancellation.
- Concurrent health updates and clearance CAS on SQLite and PostgreSQL, old selection/solve/probe rejection, quality quarantine and business-error classification.
- Cache saturation with a live stream, socket/waiter capacity, repeated socket batches and real HTTP latency/resource samples.
- Default final-review regressions for late/delayed/mixed failures, concurrent cooldown extension and recovery, pending local overlays, and pinned-download client admission, EOF, close, cancellation, shutdown and TLS/configuration errors.
- Default second-review regressions for session/environment policy, local overload without false cooldown, complete probe ownership, late and queued 403 isolation, and atomic cookie/generation capture.

Local tests do not establish production supplier availability or validate every external VLESS/Reality/VMess deployment. Protocol normalization/profile tests and the local transport tests are separate from external interoperability claims. Microbenchmarks exclude provider latency and authoritative quality-database admission; the end-to-end local HTTP test reports that boundary explicitly.

`TestRuntimeEndToEndAuthoritativeAdmissionCost` separately measures the production quality adapter against a real SQLite database in the complete local HTTP path, and verifies that storage failure still rejects admission. Use disposable test databases for PostgreSQL verification. Keep raw measurements and execution logs outside the repository; document the actual scope and skipped dependencies in the change description.
