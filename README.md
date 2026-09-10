<p align="center">
  <img alt="Grok2API" src="./frontend/public/grok2api.png" width="720" />
</p>

<p align="center">
  <strong>A multi-account API gateway for Grok Build, Grok Web, and Grok Console</strong>
</p>

<p align="center">
  English | <a href="./README.zh-CN.md">简体中文</a>
</p>

<p align="center">
  <a href="./backend/go.mod"><img alt="Go" src="https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white" /></a>
  <a href="./frontend/package.json"><img alt="React" src="https://img.shields.io/badge/React-19-61DAFB?logo=react&logoColor=111827" /></a>
  <a href="https://github.com/chenyme/grok2api/pkgs/container/grok2api"><img alt="Docker" src="https://img.shields.io/badge/Docker-amd64%20%7C%20arm64-2496ED?logo=docker&logoColor=white" /></a>
</p>

<p align="center">
  <a href="https://trendshift.io/repositories/19868?utm_source=repository-badge&amp;utm_medium=badge&amp;utm_campaign=badge-repository-19868" target="_blank" rel="noopener noreferrer"><img src="https://trendshift.io/api/badge/repositories/19868" alt="chenyme%2Fgrok2api | Trendshift" width="250" height="55"/></a>
</p>

> [!TIP]
> Check out [DEEIX-AI / DEEIX-Chat](https://github.com/DEEIX-AI/DEEIX-Chat), a lightweight, integrated AI platform for model routing, chat, files, tools, billing, identity, and operations.

> [!NOTE]
> This project is for technical research and learning purposes only. Please comply with Grok's official terms of use and local laws when using it; otherwise, you will be solely responsible for all consequences!

## Sponsors
> [Want to sponsor this project?](mailto:chenyme03@gmail.com)

<table>
<tr>
<td width="200" align="center" valign="middle"><a href="https://www.krill-ai.com/register?invite=KJ2VGIRVAE"><img src="https://raw.githubusercontent.com/Krill-ai-org/krill-ai-static/refs/heads/main/krill-logo/Eng/250x150.png" alt="Krill AI" width="160"></a></td>
<td valign="middle">Krill AI provides fast, stable API access to GPT, Claude, Gemini, and leading Chinese models, with enterprise customization, invoicing, 7×16 support, and optimized WebSocket connections for faster first-token latency. Register through the <a href="https://www.krill-ai.com/register?invite=KJ2VGIRVAE">exclusive link</a> and use code “grok2api” for 23% off your first Codex package.</td>
</tr>
<tr>
<td width="200" align="center" valign="middle"><a href="https://github.com/DEEIX-AI/DEEIX-Chat"><img src="frontend/public/sponner/deeix-chat_deeix-ai.png" alt="DEEIX AI / DEEIX Chat" width="160"></a></td>
<td valign="middle">DEEIX-Chat is an open-source, self-hostable AI Chat platform for individuals, teams, and enterprises that need stable, long-term, unified access to multiple models. It brings models, conversations, files, tool calling, and administration together in one deployable and extensible system. Click <a href="https://github.com/DEEIX-AI/DEEIX-Chat">here</a> to start deploying.</td>
</tr>
<tr>
<td width="200" align="center" valign="middle"><a href="https://www.right.codes/register"><img src="frontend/public/sponner/rightcode.jpg" alt="RightCode" width="160"></a></td>
<td valign="middle">Right Code is an enterprise-grade AI Agent distribution platform that primarily provides stable access services for Claude Code, Codex, Gemini, and other models. It supports invoicing and dedicated one-to-one assistance for enterprises and teams. Thanks to Right Code for providing token support. Click <a href="https://www.right.codes/register">here</a> to register and get started.</td>
</tr>
<tr>
<td width="200" align="center" valign="middle"><a href="https://api.fenno.ai/s/xCBS"><img src="frontend/public/sponner/fenno-ai.jpg" alt="FennoAI" width="160"></a></td>
<td valign="middle">FennoAI provides enterprise-grade OpenAI/Anthropic-compatible APIs for Codex, Claude Code, and OpenCode, processing hundreds of billions of tokens daily with global business settlement and invoicing. Through the Grok2API <a href="https://api.fenno.ai/s/xCBS">exclusive offer</a>, USD 1.99 unlocks USD 50 in Coding Plan credits, plus referral commissions up to 20%.</td>
</tr>
<tr>
<td width="200" align="center" valign="middle"><a href="https://s.qiniu.com/RNNZFf"><img src="frontend/public/sponner/qiniu.jpg" alt="Qiniu Cloud AI" width="160"></a></td>
<td valign="middle">Qiniu Cloud AI, Qiniu Cloud’s (02567.HK) enterprise MaaS platform, offers protocol-compatible access to 150+ global models for text, image, audio, video, and files, serving 1.69+ million users. Grok2API registrations through the <a href="https://s.qiniu.com/RNNZFf">exclusive link</a> receive 12 million free enterprise tokens or 3 million developer tokens.</td>
</tr>
</table>

<br>

## Overview

Grok2API is a Go gateway with a built-in React admin console. It manages independent Grok Build, Grok Web, and Grok Console account pools and exposes unified OpenAI- and Anthropic-compatible APIs.

### Architecture

See the [architecture and module boundaries](ARCHITECTURE.md), [development and customization guide](DEVELOPMENT.md) (Chinese), and [AI contributor instructions](AGENTS.md) for feature locations, extension contracts, validation, migrations, and repository privacy requirements.

```mermaid
flowchart LR
    %% Color definitions
    classDef access fill:#e1f5fe,stroke:#01579b
    classDef core fill:#fff3e0,stroke:#e65100
    classDef providers fill:#f3e5f5,stroke:#4a148c
    classDef infra fill:#e8f5e9,stroke:#1b5e20
    classDef upstream fill:#fce4ec,stroke:#880e4f

    subgraph Access["Access Domain"]
        direction LR
        Clients["API Clients"]
        Admin["React Admin"]
    end

    subgraph Core["Gateway Core Domain"]
        direction LR
        Management["Management Services<br/>Accounts · Models · Keys · Settings"]
        Sync["Account Sync<br/>Credentials · Quota · Models"]
        Gateway["Gateway Service<br/>Protocols · Routing · Selection · Retry"]
        Audit["Audit Service<br/>Usage · Client Billing"]
        Management --> Sync
        Gateway -.-> Audit
    end

    subgraph Providers["Provider Channel Domain"]
        direction LR
        Registry["Provider Registry"]
        Build["Grok Build<br/>OAuth · Dynamic Models · Billing"]
        Web["Grok Web<br/>SSO · Remote Quota · Media"]
        Console["Grok Console<br/>SSO · Local Window · Stateless"]
        Registry --> Build
        Registry --> Web
        Registry --> Console
    end

    subgraph Infra["Shared Infrastructure Domain"]
        direction LR
        Egress["Egress Manager<br/>Scopes · Proxy Pool · Fallback · Clearance"]
        Database[("SQLite / PostgreSQL")]
        Runtime[("Memory / Redis")]
    end

    Upstream["🌐 Grok Upstream"]

    %% Cross-domain calls
    Clients --> Gateway
    Admin --> Management
    Gateway --> Registry
    Sync --> Registry
    Build -->|grok_build| Egress
    Web -->|grok_web / asset| Egress
    Console -->|grok_console| Egress
    Egress --> Upstream
    Management --> Database
    Audit --> Database
    Gateway <--> Runtime

    %% Application styles
    class Clients,Admin access
    class Management,Sync,Gateway,Audit core
    class Registry,Build,Web,Console providers
    class Egress,Database,Runtime infra
    class Upstream upstream
```

The Gateway routes requests through the Provider Registry. Account Sync refreshes credentials, quota, and models. Each Provider keeps independent account state and uses an isolated egress scope; usage, audits, and client billing are finalized after the request.

### Core capabilities

| Area | Capabilities |
| :-- | :-- |
| APIs | Responses, Chat Completions, Anthropic Messages, Images, and asynchronous Videos |
| Clients | Codex, Claude Code, OpenAI-compatible SDKs, and Anthropic-compatible SDKs |
| Accounts | Bulk import/export, quota sync, credential renewal, conversion, tools, and cleanup |
| Routing | Model discovery, Provider pinning, sticky sessions, quota/concurrency guards, and bounded failover |
| Sessions | Stored responses, compact, prompt-cache affinity, and optional reasoning replay |
| Media | Image generation/editing, video jobs, local archiving, and URL/Base64/SSE output |
| Egress | HTTP/SOCKS/Resin and Trojan/VLESS/Shadowsocks/VMess tunnels, subscriptions, probes, proxy pools, allocation, fallback, per-traffic-class egress routing for Grok Build, and FlareSolverr |
| Operations | Dashboard, model routes, client keys, audits, runtime settings, and media libraries |

### Provider boundaries

| Provider | Authentication | Models | Main capabilities |
| :-- | :-- | :-- | :-- |
| Grok Build | OAuth / Device OAuth | Discovered per account | Responses, Chat, Messages, compact, stored responses, paid-account video |
| Grok Web | SSO | Built-in, filtered by tier | Responses, Chat, Messages, stored responses, images, image editing, video |
| Grok Console | SSO | Built-in | Stateless Responses, Chat, Messages, images, image editing, video, TTS, STT, Realtime |

Each Provider keeps its own credentials, quota, health, cooldown, concurrency, and model capabilities. Account retries stay within one route; when one public model ID intentionally aggregates multiple routes, the gateway may select another schedulable route without mixing Provider state.

## Quick start

Official images support `linux/amd64` and `linux/arm64`.

```bash
git clone https://github.com/chenyme/grok2api.git
cd grok2api
cp config.example.yaml config.yaml
```

Generate secrets and place them in `config.yaml`:

```bash
openssl rand -hex 32
openssl rand -base64 32
```

```yaml
secrets:
  jwtSecret: "replace-with-the-generated-hex-value"
  credentialEncryptionKey: "replace-with-the-generated-base64-key"

bootstrapAdmin:
  username: "admin"
  password: "replace-with-a-strong-password"
```

Start the service:

```bash
docker compose pull
docker compose up -d
docker compose logs -f grok2api
```

Open `http://127.0.0.1:8000`. The image already includes the frontend; SQLite data and local media are stored in the Compose volume.

### Run from source

```bash
cp config.example.yaml config.yaml
make run
```

For frontend development:

```bash
cd frontend
pnpm install
pnpm dev
```

## Set up the gateway

1. Sign in with the bootstrap administrator.
2. Connect a Build, Web, or Console account.
3. Wait for its quota and model capabilities to sync.
4. Review the public routes under **Model Routes**.
5. Create a client key under **Client Keys**.
6. Call a `/v1/*` endpoint with that key.

After first sign-in, change the administrator password and remove `bootstrapAdmin` from the configuration. Never rotate `credentialEncryptionKey` after credentials have been stored.

### Account operations

| Provider | Connect or import | Export |
| :-- | :-- | :-- |
| Build | Device OAuth, JSON/JSONL | Re-importable account file |
| Web | Pasted/TXT SSO, JSON/JSONL | Re-importable account file |
| Console | Pasted/TXT SSO, JSON/JSONL | Re-importable account file |

Imports accept UTF-8 BOM. Bulk quota sync, Build credential renewal, Web→Build/Console conversion, account tools, and cleanup report live progress.

Build refresh tokens may rotate when renewed. Do not actively share one Build credential between grok2api, the official CLI, another gateway, or another independent client: one client can consume a token that another client still holds. Authorize each active client separately, or transfer the credential only after the previous client has stopped using it.

Web account tools can accept the terms, set a random birthday corresponding to an age of 20–40, and enable NSFW. Completed steps are recorded and skipped on later runs.

Automatic deletion of old `reauthRequired` accounts is available but disabled by default. Active inference leases and video jobs are protected.

> [!TIP]
> To migrate from the Python version, export Grok Web SSO tokens as TXT and import them under **Grok Web**. Old pool metadata and databases are not compatible.

## Models and routing

Build models are discovered from each account's actual capabilities. Web and Console use built-in catalogs. The **Model Routes** page shows Provider-qualified routes, endpoint capabilities, and supporting-account counts; clients should treat the currently serviceable results from `GET /v1/models` as authoritative.

### Grok Build

Build does not use one global static model list. Account synchronization reads the upstream `/models` endpoint, and different accounts, subscription tiers, or staged rollouts may expose different models. Routing retains these per-account capabilities instead of replacing the global catalog with one account's response.

| Model | Type | Availability | Gateway surfaces |
| :-- | :-- | :-- | :-- |
| Conversation models returned by upstream `/models` (for example, `grok-4.5`) | Conversation | Returned by the selected account | Chat Completions, Responses, Messages, compact, stored responses |
| `grok-composer-2.5-fast` | Conversation | Grok Build OAuth accounts | Chat Completions, Responses, Messages; supplemented from the OAuth session contract when a sparse upstream catalog omits it |
| `grok-imagine-video-1.5` | Video | Super/paid Build accounts | Videos; not assigned to Free or unknown-entitlement accounts |

Conversation requests are translated to the Build Responses protocol while preserving the tool, reasoning, multi-turn, and prompt-cache compatibility required by Codex and Claude Code. Build currently exposes no image generation or image editing routes.

### Grok Web

Web uses a built-in catalog filtered by account tier; higher tiers inherit lower-tier models.

| Model | Type | Minimum tier | Gateway surfaces |
| :-- | :-- | :-- | :-- |
| `grok-chat-fast` | Conversation | Basic | Chat Completions, Responses, Messages |
| `grok-chat-auto` | Conversation | Super | Chat Completions, Responses, Messages |
| `grok-chat-expert` | Conversation | Super | Chat Completions, Responses, Messages |
| `grok-chat-heavy` | Conversation | Heavy | Chat Completions, Responses, Messages |
| `grok-imagine-image-lite` | Image | Basic | Images Generations |
| `grok-imagine-image` | Image | Basic | Images Generations (`enable_pro=false`) |
| `grok-imagine-image-2.0` | Image | Basic | Images Generations (`enable_pro=true`) |
| `grok-imagine-image-edit` | Image Edit | Basic | Images Edits |
| `grok-imagine-video` | Video | Basic for 720p; Super for 480p | Videos |

Web Imagine generation maps `aspect_ratio` and `n` to the browser protocol. `size` remains an OpenAI-compatible aspect-ratio alias, while generation-only `resolution` and `quality` are ignored on Web routes because the upstream product is selected by the model name rather than by those Console-oriented controls.

### Grok Console

Console uses the catalog built into the current release. Conversation forwarding is stateless, while image, video, and voice use the standard xAI resource APIs.

| Model | Type | Gateway surfaces |
| :-- | :-- | :-- |
| `grok-4.20-0309-non-reasoning` | Conversation | Chat Completions, Responses, Messages |
| `grok-4.20-0309-reasoning` | Conversation | Chat Completions, Responses, Messages; the model reasons but the upstream rejects configurable `reasoningEffort` |
| `grok-4.20-multi-agent-0309` | Conversation | Chat Completions, Responses, Messages |
| `grok-4.5` | Conversation | Chat Completions, Responses, Messages |
| `grok-4.3` | Conversation | Chat Completions, Responses, Messages |
| `grok-build-0.1` | Conversation | Chat Completions, Responses, Messages |
| `grok-imagine-image` | Image, Image Edit | Images Generations, Images Edits |
| `grok-imagine-image-quality` | Image, Image Edit | Images Generations, Images Edits |
| `grok-imagine-image-2.0` | Image, Image Edit | Images Generations, Images Edits |
| `grok-imagine-video` | Video | Videos |
| `grok-imagine-video-1.5` | Video | Video generation, including Free Console accounts |
| `grok-voice-latest`, `grok-voice-think-fast-2.0`, `grok-voice-think-fast-1.0` | Voice | TTS and Realtime WebSocket proxy |
| `grok-stt` | Voice | STT and OpenAI-compatible audio transcriptions |

Generation and editing capabilities for the same Console image model are grouped into one logical model row; no separate `-edit` model copy is required.

Public names normally omit the Provider. Internally, routes use `Build/`, `Web/`, or `Console/`; qualified names can pin a request to one source.

Web can be weakly linked one-to-one with matching Build and Console accounts. Links share only an anonymous egress identity and provenance display. They never merge credentials, quota, health, cooldown, concurrency, capabilities, or billing.

### Codex, Claude Code, and prompt caching

Responses and Messages support streaming, tools, reasoning, multi-turn sessions, and compaction. Stable client session signals are preserved for Grok Build prompt-cache affinity. Cache hits still require a compatible upstream account and an unchanged prompt prefix. A still-decryptable compaction summary from this gateway instance is expanded even if the session or PromptCacheKey remaps; foreign or undecodable blobs remain a compatibility boundary.

Responses and Chat Completions report OpenAI-style total input. Messages reports Anthropic-style uncached input and cache reads separately. Audits retain total and cached input for billing reconciliation.

## API

Inference endpoints use a client key:

```http
Authorization: Bearer g2a_xxx_xxx
```

| Method | Path | Purpose |
| :-- | :-- | :-- |
| `GET` | `/healthz`, `/readyz` | Liveness and readiness |
| `GET` | `/v1/models` | Serviceable models |
| `POST` | `/v1/responses` | Responses JSON/SSE |
| `POST` | `/v1/responses/compact` | Compact a supported Response session |
| `GET`, `DELETE` | `/v1/responses/{id}` | Read or delete a stored response |
| `POST` | `/v1/chat/completions` | Chat Completions JSON/SSE |
| `POST` | `/v1/messages` | Anthropic Messages JSON/SSE |
| `POST` | `/v1/images/generations`, `/v1/images/edits` | Generate or edit images |
| `POST`, `GET` | `/v1/videos/*` | Create and inspect video jobs |
| `POST` | `/v1/tts`, `/v1/audio/speech`, `/v1/audio/tasks` | Synthesize speech |
| `POST` | `/v1/stt`, `/v1/audio/transcriptions` | Transcribe audio |
| `GET` | `/v1/stt`, `/v1/realtime` | Proxy voice WebSocket sessions |
| `GET` | `/v1/media/images/{asset_id}`, `/v1/media/videos/{asset_id}` | Read archived media |

Streaming STT preserves audio configuration query parameters and repeated `keyterm` values. For example, connect to `/v1/stt?model=grok-stt&encoding=pcm&sample_rate=48000&multichannel=true&channels=2`. Invalid or unsupported options return HTTP 400 before an upstream connection is attempted; each configured channel must report completion before the request is recorded as fully generated.


Stored responses and compact depend on the selected Provider and model. The signed-in admin console provides live examples at `/docs`; Swagger is available only when `server.swaggerEnabled: true`.

For Responses, explicit `store:false` makes the new response ID unavailable through `GET/DELETE` and `previous_response_id`. The request can still use an existing stored parent. Web text Responses save resources when `store` is omitted, `null`, or `true`; Build forwards an explicit storage choice upstream, and Console remains stateless. Usage/audit records and the separately configured Build conversation history follow their own retention settings.

Web image models can return generated images through Responses, Chat Completions, and Messages. Their response IDs are temporary: Responses returns `store:false`, and `GET/DELETE` or `previous_response_id` cannot recover a conversation from them. Explicit `store:true` is rejected before generation. The `image_config` extension (for example, `{"n":2,"response_format":"b64_json"}`) is preserved across these protocols. Budget-limited keys reserve the requested image cost before generation and settle from confirmed images, including when a later download or client delivery fails.

Continuing a stored response via `previous_response_id` can be rejected by the upstream organization (HTTP 404, `upstream_server_error_not_found`) — some Grok organizations disallow cross-request conversation reuse. The gateway pins the original account and forwards the session correctly; a 404 here reflects the upstream policy, not lost state. `GET/DELETE /v1/responses/{id}` still work for gateway-stored responses.

`/v1/audio/transcriptions` supports `json` (default), `verbose_json`, and `text`. Video edit/extension routes must resolve to Console `grok-imagine-video`; custom public model names remain supported. Monetary billing is applied only when the gateway can reliably measure the official pricing unit: TTS is reserved and settled from its input character count, while REST and streaming STT are settled from the actual audio duration returned by a successful response. Because STT duration is known only after completion, concurrent requests may briefly take a billing-limited key beyond its spend limit. Realtime, video edits/extensions, and custom routes without a recognized official price are currently audited as unpriced; they remain callable and do not consume the spend limit.

Client keys support model allowlists and optional RPM, concurrency, spend, and expiry limits.

```bash
curl http://127.0.0.1:8000/v1/responses \
  -H "Authorization: Bearer g2a_xxx_xxx" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "your-model",
    "input": "Explain quantum tunneling in three sentences.",
    "stream": true
  }'
```

## Egress and Cloudflare

Egress nodes are pure proxy resources - no scope, no account binding. The admin console supports:

- HTTP, HTTPS, SOCKS4/4A, SOCKS5/5H, Resin, Trojan, VLESS, Shadowsocks, and VMess
- TCP, WebSocket, and TLS tunnel transports; unsupported variants are rejected during import
- Subscription and text/Base64 import
- Batch probes, filtering, and deletion
- Three-level exit routing resolved per request: traffic class (inference / credential / billing / model sync / video) -> scope (Build / Web / Console) -> default exit -> automatic schedule. Each level can be unset (follows the next level down), direct, one fixed node, or a dedicated pool. "Falling back" only means an unset level resolving to the next level down; a configured target is a strict binding — when its node is quarantined/cooling/disabled or its pool is exhausted, requests fail fast with an explicit error instead of silently rerouting to other exits. Use a pool for fault tolerance: member rotation, chained pools, and in-pool direct fallback all stay inside the configured boundary
- Dedicated pools: named node groups with their own scheduling strategy (caller-sticky rendezvous, random, first-preferred, forward rotation) and an exhausted-fallback (another pool or direct)
- Proxy-pool mode without global cooldown after one connection failure
- Immediate recovery probes after fixed-proxy transport failures, with per-node coalescing and bounded waiting for fast retry
- Give each sticky session its own fixed node (`proxyPool=false`). Do not merge several stickies into one node, or a failing session can only be found by taking down the whole group

Hysteria and TUIC are not supported yet. FlareSolverr accepts only HTTP/SOCKS proxy URLs, so automatic clearance refresh cannot use a tunnel share URL directly.

The real-time response guard uses `requestRetry` in `config.yaml` as its initial baseline. After the first administration save, the versioned `quality_guard` record is authoritative. The Go baseline is disabled; the example configuration enables it:

```yaml
requestRetry:
  enabled: true
  maxAttempts: 2
  guardedModels: ["grok-4.5", "grok-4.6"]
  createdTimeout: 5s
  evidenceTimeout: 3.5s
  onExhausted: fail_closed
  accountCooldown: 2m
  idleAccountCooldown: 15m
```

Select the actual public model names in the guard page. An empty bootstrap list uses the default grok-4.5/grok-4.6 selection; saving an enabled guard requires at least one model. Settings saves include a revision and reject stale edits. Each request keeps one immutable snapshot of scope, rules and budgets. Configuration or kernel failures are explicit errors.

Visible thinking satisfies admission. A closed reasoning stage without visible thinking, or visible answer/refusal text arriving first, is withheld. Ciphertext and usage counters cannot provide thinking evidence. Unknown protocols, resource exhaustion, timeouts, empty streams and cancellation have separate outcomes. Chat supports one choice with index 0; guarded `n > 1` requests are rejected before generation. Exhausted quality retries return `503 upstream_degraded`.

Admission has a total deadline across account selection, response headers, inspection, conversion and retries: 30 seconds by default, or 3 minutes when tools are present. Search and heavy reasoning can relax individual silence deadlines while remaining subject to the total deadline. Client delivery commits before response headers are sent; complete delivery is recorded separately. Conversation output is committed only after successful delivery and the required durable completion receipt. A stream interrupted after its first thinking delta cannot count as a successful rescue.

Build and Console SSE consumers share one event assembler. Buffer allocation and parser/converter state reserve request and process capacity before processing: 96 MiB per request and 512 MiB per process by default, with separate prefix, event and JSON limits. These are accounted response capacities, not an RSS limit. Capacity failure returns `response_resource_exhausted` without a quality vote or account rotation. Automatic replay requires a safe tool policy; unknown server-side tool effects block regeneration.

Admission, completion and physical HTTP exchanges are separate durable facts. Each transport attempt retains its original account, route epoch, policy revision and rule version, including internal retries. Physical usage distinguishes unreported counters from reported zero. Temporary restrictions are owned by their source events or cases; releasing one does not release another. A degradation event initially holds an account for 2 minutes without changing its manual enabled state. Durable receipt failure stops retries and leaves bounded local protection. The outbox defaults to 10,000 pending events, and `/guard-stats` exposes backlog and resource capacity.

See the [response guard architecture](backend/internal/quality/guard/README.md) for protocol rules, resource limits, migration and validation commands.

### Quality attribution and exit handling

A withheld response starts temporary protection and a controlled comparison investigation. It does not identify the account or exit as the cause. Build investigations compare other accounts on the incident exit, the incident account on other verified paths, and matching healthy controls. Missing results and transport failures do not become degradation votes. Conclusions distinguish account quality, account availability, exit/IP issues and insufficient evidence.

Investigations have fixed sample budgets and deadlines. Restrictions belong to their cases and exit epochs; unrelated cases and manual account state remain independent. The administration page shows supporting evidence, counterevidence, path verification and manual review. A probe must observe successful completion within its budget before it can provide a clean result. A single clean probe on another product surface is not an attribution verdict.

See the [controlled comparison protocol](backend/internal/quality/README.md) for the decision rules and operating limits. The current investigation worker assumes one investigation process per database. Exit routing and rotation remain separate operational mechanisms; the node editor accepts rotation webhooks and `scripts/rotate-server/` contains the server implementation.

### Request audits



Every inference request lands in the audit ledger (request_audits) with per-attempt
diagnostics (request_audit_attempts), including quality-guard retries (stages
`quality_hold` for withheld attempts and `quality_idle` for empty/evidence/created-timeout aborts).
Two observability columns answer "how much did the client actually receive":

- `deliveredEvents / deliveredBytes` - SSE data events forwarded and cumulative
  bytes written to the client (non-streaming: body bytes). A 200-with-error-code
  row now states exactly what reached the client; both fields are exposed in the
  admin request-audits API and the audits page performance summary.

Retention uses one setting, `audit.retentionPeriod`: default `168h` (7 days),
`0` keeps records indefinitely, and non-zero values must be between `24h` and
`8760h`. One worker deletes aged audits and attempt details hourly, with at most
500 records per transaction and a 30-second sweep budget. It reads durable
settings before every batch; a saved change applies to the next batch even when
an instance misses a notification. An already started transaction may finish
under its previous policy. A policy read or delete failure stops the sweep.

The loader accepts legacy `retention` / `retentionDays` files and merges their
enabled windows. Persisted legacy online days take precedence, including zero.
Do not mix those file keys with the new key. The management page shows effective
and file baseline sources, and preserves fractional days without rounding.
See [audit retention compatibility](DEVELOPMENT.md#审计保留的兼容要求)
for precedence, old client compatibility, and rollout constraints.


### Verification matrix


The backend ships a one-shot verification script consolidating the review
gates established during hardening:

- **fast** (`make verify`): build, vet, staticcheck, race-enabled test suite.
- **full** (`make verify-full`): + fuzz seed regressions, govulncheck,
  and a count=3 flaky probe over the seven timing-sensitive packages
  (gateway, risk, rsc, relational, app, inference, jsonpeek).
- **fuzz** (`make fuzz`): 30s of the mutation engines per parse target
  (SSE quality scanner + body peek, RSC payload parser, jsonpeek
  extractors, egress subscription payloads — 7 targets in total).

Third-party tools degrade to SKIP with install hints when absent.
See [HARDENING.md](./HARDENING.md) for the complete hardening log: detection rules, attribution flow, cooldown taxonomy, security fixes, and production measurements.

### Cooldown taxonomy

Three independent cooldown families are visible in the admin UI:

- **Routing guard** (`requestRetry.accountCooldown` / `idleAccountCooldown`):
  `missing_thinking` (first strike cools; a later strike after expiry disables
  the account), `missing_thinking_disabled`, and `quality_idle_timeout`
  (empty/silent stream, kept separate from the failure counter and with its own
  configurable duration). A clean RSC verdict lifts these. The admin UI shows a
  clear action on a cooling badge as the manual operator escape hatch.
- **Routing cooldown** (`routing.cooldownBase`/`cooldownMax`): generic upstream
  failures with exponential backoff; never cleared by risk attribution.
- **Egress-node cooldown**: exponential backoff and health re-probes for fixed-node
  transport failures; independent of account state.

The request-audits page filters by error code (`quality_degraded`) and account
rows show the cooldown reason on hover, so degraded-withhold events can be
diagnosed end to end.

Resin usernames can contain `{account}`:

```text
socks5h://Default.{account}:RESIN_PROXY_TOKEN@resin:2260
```

The placeholder becomes a stable anonymous identity. Linked Web, Build, and Console accounts can share it; raw tokens and email addresses are not used.

For managed Web/Console Cloudflare Clearance (uncomment the `flaresolverr` service block in `docker-compose.yml` first — it ships commented as an optional template):

```bash
docker compose --profile flaresolverr up -d
```

Then use `http://flaresolverr:8191` under **Runtime Settings → Media & Network → Clearance** and select one of the managed modes:

- `FlareSolverr` proactively refreshes stale fixed-egress Clearance on the configured schedule.
- `On demand` keeps the last successful Clearance regardless of age and solves again only after an upstream rejection explicitly invalidates it. Scheduled refresh does not launch a browser in this mode.

`Manual` never invokes FlareSolverr. The on-demand mode can make the first request without a managed Clearance; if Cloudflare rejects it, the next lease performs one deduplicated solve.

The egress layer retries only connection failures known to occur before request submission. It does not replay submitted generation requests, authentication failures, exhausted quotas, or upstream rate limits.

When a fixed proxy enters cooldown after a transport failure, grok2api starts an independent connectivity probe immediately. Concurrent failures share one probe. A later request bound to that node waits for at most five seconds, reloads persisted node state after a healthy probe, and continues without waiting for the full cooldown. An unhealthy probe preserves the cooldown. Proxy-pool leases use fresh tunnels, so one rotating exit failure never cools the whole pool. See [Immediate egress failure probe and bounded retry](./backend/internal/infra/egress/FAILURE_RETRY.md) for the design and safety invariants.

## Configuration and deployment

`config.yaml` contains startup settings; Provider and operational settings are managed in the admin console and hot-reload unless marked otherwise.

| Deployment | Database | Runtime store | Media |
| :-- | :-- | :-- | :-- |
| Single instance | SQLite | Memory | Local directory |
| Multiple instances | PostgreSQL | Redis | Shared read/write directory |

Multi-instance deployments require a unique `deployment.instanceID` per replica, one shared `clusterID`, and `sharedMedia: true` only after the media directory is shared correctly.

PostgreSQL credentials can be injected without storing them in `config.yaml`:

```bash
GROK2API_DATABASE_URL='postgresql://user:password@host:5432/grok2api?sslmode=require' docker compose up -d
```

A non-empty `GROK2API_DATABASE_URL` overrides `database.postgres.dsn` and automatically selects the `postgres` driver. An empty value is ignored. Supported URL schemes are `postgres://` and `postgresql://`; SQLAlchemy's `postgresql+asyncpg://` form is rejected with a migration hint. The application does not implicitly read the generic `DATABASE_URL`; platforms that provide it can map it explicitly with `GROK2API_DATABASE_URL: "${DATABASE_URL}"`. Database configuration precedence is built-in defaults, `config.yaml`, then `GROK2API_DATABASE_URL`. The current CLI has no database override.

### Admin session security

Admin tokens are opaque and validated against the session store on **every request** — revoking a session kills its access tokens immediately (not at JWT expiry). Refresh tokens rotate on every use in an HttpOnly/Secure/SameSite=Strict cookie scoped to the auth path. Replaying a rotated refresh token is treated as theft per OAuth BCP (RFC 6819 §5.2.11): replays inside a 30 s grace window are tolerated as benign duplicate refreshes (concurrent-client race), while later replays revoke the **entire token family** — both generations stop working at once. Password change revokes all sessions of that admin.

### Graceful shutdown

On `SIGTERM`/`SIGINT`, the application stops accepting requests and gives existing HTTP and WebSocket handlers a **15 s drain window**. Their network dependencies remain available so an accepted request can finish its remaining upstream steps. At the deadline, the application cancels request contexts, closes remaining connections (including upgraded WebSockets), and waits up to 10 s for handler completion. Known generation usage and billing still pass through their completion paths. Accepted video jobs retain their durable recovery state.

The application then joins its background tasks, stops quality and network workers, closes the observation recorder and audit writer, and finally closes runtime and database connections. The whole observation queue gets one 5 s drain budget; unpersisted observations contribute to the existing drop counter. Accepted billing facts remain in the durable audit journal for recovery when SQL is unavailable.

An expired HTTP drain window alone is a normal stop. A handler or worker that has not actually stopped is a shutdown error: its dependencies remain open for completion, and the CLI reports the error. `Application.Close` also supports stopping an active `Run`, concurrent callers, and retry after an incomplete close. Successful construction must always be paired with `Close`, including when `Run` fails.

The Compose example allows **90 s** for the complete sequence. This replaces the old 26 s worst-case claim, which omitted several workers and did not wait for long-stream completion. The limit gives cooperative cleanup room; a process killed by its orchestrator cannot finish an unacknowledged external operation. Other deployment managers should allow a comparable grace period. This repository change does not modify running deployments.

Shutdown-related logs:

- `server_started`, `server_stopping` (`uptime_ms`), and `server_stopped` (`drain_ms`) describe the HTTP lifecycle.
- `server_shutdown_drain_timeout` records forced cancellation after the HTTP grace period.
- `quality_observations_stopped_with_drops` reports the recorder's cumulative drop count when nonzero.
- `application_closed` confirms successful worker and dependency cleanup.

### Client IPs behind a reverse proxy

Request audits record the normalized client IPv4 or IPv6 address. Direct deployments need no extra configuration. Behind Nginx or another reverse proxy, configure both sides:

> When the public port differs from the internal one (port mapping or a proxy),
> also set `frontend.publicApiBaseURL` (config file or the admin settings page,
> hot-applied) — media download URLs are built from it and otherwise point at the
> internal `127.0.0.1:8000` default, unreachable for clients.

1. Forward the standard client IP headers from the proxy:

```nginx
location / {
    proxy_pass http://127.0.0.1:8000;

    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

2. Trust only the proxy address or its isolated network in `config.yaml`:

```yaml
server:
  trustedProxies:
    - "127.0.0.1"
```

With Docker, the peer seen by grok2api may be the bridge gateway or another container rather than `127.0.0.1`. Inspect the Compose network before configuring it:

```bash
docker network inspect grok2api_default \
  --format '{{(index .IPAM.Config 0).Subnet}}'
```

For example, an isolated network reported as `172.20.0.0/16` can be configured as a trusted proxy CIDR. Never use `0.0.0.0/0` or `::/0`; grok2api rejects unrestricted trusted-proxy ranges. Without `trustedProxies`, forwarded headers are ignored and audits contain the direct TCP peer address, preventing clients from spoofing `X-Forwarded-For`.

If Cloudflare is in front of Nginx, configure Nginx's real-IP module with `CF-Connecting-IP` and Cloudflare's official proxy ranges first. Do not trust `CF-Connecting-IP` from arbitrary peers. Restart grok2api after changing `server.trustedProxies`; reload Nginx after changing its configuration.

Important optional settings:

- `audit.journalDirectory`: persistent local directory for accepted audit records awaiting SQL settlement. Keep the directory and deployment instance identity stable across restarts; each instance owns a separate file. SQLite WAL requires local storage, not a network filesystem.
- `audit.journalMaxBytes` and `audit.bufferSize`: bound retained payload bytes and pending record count. They include records awaiting repair, never evict accepted facts, and require restart to change. Budget additional disk space for SQLite indexes and WAL.
- `audit.ledgerMode`: `observe` reports recoverable ledger backlog; `enforce` pauses new inference after the configured grace. Both modes block after unaccepted facts or retained invalid records. SQL recovery safely replays accepted records; invalid records are retained and retried once at startup after repair.
- Billing reservations retain their deployment instance owner. Only that instance can expire its inactive reservations after restoring its journal; another instance can still settle a real event. Legacy reservations with unknown owners remain reserved until a real settlement or explicit request cancellation. Keep stable instance identities and persistent volumes, and stop old cleanup processes before upgrading. Client-key management shows pending reservations separately from billed usage.
- `routing.accountIsolatedConnections`: partitions outbound TCP/HTTP pools by account for external L4 or connection-hash load balancers. It is off by default because it increases connections, TLS handshakes, memory, and file-descriptor usage.
- `routing.segmentedSelectorEnabled`: enabled by default for pools with at least 3,000 eligible accounts; bounds dynamic concurrency reads while retaining quota/tier priorities, sticky sessions, full-planner fallback, and atomic guards.
- Build response-header timeout and exact-match 403 invalidation rules are hot-reloadable.
- **Sync latest version** applies the validated Grok Build client version and User-Agent.

## Production checklist

- Use HTTPS and enable `auth.secureCookies`.
- Keep Swagger disabled on public deployments.
- Use strong, backed-up secrets; never commit credentials, cookies, exports, or databases.
- Back up `config.yaml`, the database, and media storage.
- Use PostgreSQL, Redis, and shared media for multiple instances.
- Put a reverse proxy and access controls in front of public deployments.

### Backup and restore

The SQLite database runs in WAL mode — take a consistent online snapshot instead of copying database files under a running instance. Source deployments:

```bash
sqlite3 data/backend.db ".backup 'backups/grok2api-$(date +%F).db'"
```

Docker deployments ship no sqlite3 in the runtime image; snapshot through an ephemeral sidecar that shares the container's volumes (`grok2api` is the default compose container name):

```bash
docker run --rm --volumes-from grok2api -v "$PWD/backups:/backup" alpine:3.23 \
  sh -c 'apk add --no-cache sqlite >/dev/null \
    && sqlite3 /app/data/backend.db ".backup /backup/grok2api-$(date +%F).db"'
```

Back up together with:

- `config.yaml` — losing `secrets.credentialEncryptionKey` makes stored account credentials undecryptable; changing `secrets.jwtSecret` invalidates every issued session. Never commit or share these values.
- `data/media/` when using the local media driver (inside Docker it lives in the same `/app/data` volume).
- PostgreSQL deployments: use `pg_dump`; Redis runtime stores: follow standard Redis persistence practice.

Restore by stopping the instance, replacing the database and media files, keeping `config.yaml` unchanged, then starting again. Account credentials can alternatively be moved between deployments with the admin export/import API (`GET /api/admin/v1/accounts/export`, cursor-stable per provider).

### Monitoring

Runtime metrics are emitted as structured JSON log lines (`msg="performance_metric"`, one per metric family) every minute — there is no HTTP `/metrics` scrape endpoint. Ship container stdout to your log pipeline and alert on `level":"WARN"` task failures plus `upstream_*`/`egress_*` metric anomalies.

## Private protocol diagnostics

Optional capture in `internal/pkg/upstreamtrace` is disabled by default and enabled through `GROK2API_UPSTREAM_TRACE_DIR`. Captures can contain conversation content and identifying metadata: use a private directory outside the repository. Never commit captures or include them in a source archive, image, public issue, or CI output. Optional corpus replay tests accept `GROK2API_TRACE_REPLAY_DIR`; maintained regression tests use minimal synthetic inputs.

## Development

Start with the [development guide](DEVELOPMENT.md) and [module map](ARCHITECTURE.md). Run `python3 scripts/check-repository.py --staged` before committing; the check inspects the Git index, not just the working copy.

```bash
cd backend
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/grok2api
```

```bash
cd frontend
pnpm install --frozen-lockfile
pnpm lint
pnpm build
```

Regenerate Swagger after changing public API annotations:

```bash
make swagger
```

## Documentation

- [简体中文 README](./README.zh-CN.md)
- [Backend guide](./backend/README.md)
- [Frontend guide](./frontend/README.md)
