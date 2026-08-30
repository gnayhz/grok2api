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

Stored responses and compact depend on the selected Provider. The signed-in admin console provides live examples at `/docs`; Swagger is available only when `server.swaggerEnabled: true`.

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

The real-time routing guard (`requestRetry`) is a top-level `config.yaml` section and is off by default:

```yaml
requestRetry:
  enabled: true             # production-recommended (Go built-in default stays off)
  maxAttempts: 2            # global request budget: initial attempt + at most one retry (hard cap 3)
  guardedModels: []          # guard whitelist (empty = all reasoning models, e.g. ["grok-4.5","grok-4.6"]); outside models exempt
  createdTimeout: 5s        # first-event deadline: abort before any SSE data event arrives
  evidenceTimeout: 3.5s     # zero-evidence deadline: anti-deadlock backstop (no thinking evidence AND no output)
  onExhausted: fail_closed # fail_open | fail_closed (fail_closed returns 503 upstream_degraded)
  sameAccountRetry: true   # pool egress only - force-disabled under direct/fixed exits
  accountCooldown: 12h     # missing-thinking conviction cooldown
  idleAccountCooldown: 15m # empty/silent-stream cooldown (independent)
```

Both deadlines **scale by request class** (the evidence-backed regime table
`qualityLivenessSchedule`, from the 2026-08 full-chain trace survey): requests
carrying server-side search tools (`web_search`/`x_search`) get NO guard
deadline - queueing/executing/thinking silences are all legitimate work, and
dead connections are bounded by the transport stream-idle timeout (Build
default 2m); `high`/`xhigh` reasoning requests get a 30s first-event budget
(upstream queueing silence measured >5s); everything else uses the defaults
above. A deadline firing only bounds queueing - it is never evidence of
degradation; that judgment always belongs to the evidence rules (the
ciphertext-item-closed-without-deltas signature et al).

When enabled, every thinking-model response (streaming and non-streaming) is judged by the zero-delay state machine: visible thinking deltas deliver instantly (the guard then steps out of the way); a reasoning item that closes without any thinking delta - the ciphertext degrade signature - withholds in 0 ms; visible output racing ahead of thinking withholds immediately regardless of length; a terminal event with neither is a defensive withhold; zero-content streams short-circuit to the idle path (short cooldown + RSC attribution + retry) instead of a missing-thinking conviction. Purely semantic output (tool calls) delivers. Exempt paths (non-reasoning operations/models, explicitly disabled reasoning, compaction) are counted per reason in guard-stats instead of passing silently. Under `fail_closed` an exhausted budget returns `503 upstream_degraded`. This section is hot-reloadable through the admin runtime-settings surface (guard page); `config.yaml` provides the boot defaults.

Every guard behavior is observable in the admin Quality Guard page: the signal-hits panel tracks the four signals (first-event deadline, zero-evidence deadline, empty stream, missing thinking) with triggered/rescued/failed counts; the exempt ledger counts requests the guard did NOT engage, by reason - the first place to look when degraded output slips through; same-account retry and both exhaustion outcomes (deliver-last/reject) summarize below, with canary verdicts in their own table. Request-audit details carry the per-attempt guard trail (quality_hold/quality_idle stages with timing), the terminal_burst class makes the whole-output-burst signature visible where the throughput column is empty, fail-open delivered rows carry a dedicated delivery marker; the audit error-code filter offers one-click presets for all four guard codes (quality degraded / zero-evidence timeout / first-event timeout / empty stream). The dashboard resource cards total cooling accounts, risk-flagged accounts, and degraded withholds in period.

### Risk attribution (RSC)

A withheld stream does not by itself prove the account is degraded - the exit
IP may be the culprit. When `accountRisk.rscCheck` is enabled, a withhold triggers
an async registration-risk check against grok.com through the linked Web SSO
identity. The check runs the SSO thinking probe (the legacy homepage payload
parse was removed with this refactor - it read every account as clean after
the grok.com redesign; rollback-to-no-attribution is the `enabled` switch):

- **ssoProbe (default, priority)**: opens one tiny temporary `fast` conversation with the
  SSO cookie (no persisted chat, no memory writes). A notetaker/thinking channel in
  the first stream means the account is healthy; answer text arriving with no
  thinking at all means the account is risk-controlled. grok.com stopped
  delivering botFlag fields through the homepage RSC payload, so this is the only
  surviving account-level signal (independent of exit-IP quality). Each check
  consumes one message of the account's rolling quota; rate limits, challenges,
  and stream errors always classify as error (retried later), never as risk.
- **buildProbe (fallback, unlinked Build accounts only)**: accounts linked to a
  Web identity always use ssoProbe; an unlinked Build that degrades gets one tiny
  reasoning request through its own credential (classified by the production
  guard signals). IP pollution is the built-in confound of that signal, so a
  degraded first attempt triggers a **differential second attempt** (pool node =
  re-roll for a new exit IP, fixed node = excluded and rerouted, direct-only =
  inconclusive error, never a denial). A double-degraded verdict additionally
  requires a recent build-probe clean witness, otherwise it is suppressed to
  error and retried. Without any reasoning Build model the fallback stays
  disabled (behavioral penalties only).

- **denied/flagged**: a confirmed verdict stays trusted for `deniedTTL` (default 24h)
  and requires `deniedConfirmations` consecutive denials (default 2) before flagging.
  Request-path attribution is **channel-scoped** — only the account that actually
  degraded gets `rsc_denied` / disabled (flag by default; disable / markOnly
  available); a Build degrade does not cascade onto SSO. **Exception: an SSO-identity
  denial** (periodic patrol, or a Web-channel degrade whose probe returns denied) fans
  out to the whole identity group (Web/Build/Console), because a flagged SSO identity
  can no longer run the probe that later Build/Console degrades need, leaving those
  channels stuck in cooldown. Flagged accounts stay enabled but are excluded from
  scheduling until an operator clears the flag, or until `deniedTTL` expires and
  patrol re-probes clean (which clears the flag, including the SSO identity group).
  The probe carries a channel-vocabulary breaker: a denied streak with zero clean
  witnesses is suppressed and self-heals by re-probing the most recent clean identity,
  so a grok.com protocol change cannot mass-flag the pool and a genuinely
  risk-controlled pool cannot deadlock the breaker.
- **clean**: the degrade was exit-IP scoped; quality cooldowns (missing-thinking,
  empty-stream idle) are lifted so the account stays schedulable, and any
  `rsc_denied` flag on that account is cleared. Generic 5xx failures are never
  cleared by a clean verdict.
- A patrol loop re-checks clean/error verdicts after `patrol.bucketDays`,
  unconfirmed denials after the error-retry window, and confirmed denials after
  `deniedTTL`. Every field in this section is editable in the
  admin UI (Guard → Risk attribution) and applies immediately after save
  (detection method, denied action, concurrency, patrol toggles included); editing
  config.yaml directly still requires a restart, and once saved in the UI the runtime
  settings take precedence.

### Exit-IP quality guard and automatic rotation

See [EXIT-IP-GUARD.md](EXIT-IP-GUARD.md) for full deployment steps (multi-instance servers, batch webhook templating, end-to-end verification).
When RSC attribution returns **clean**, the exit IP is the suspect. The exit-IP quality guard (`egress` config section) closes the loop:

- A degraded attempt excludes its egress node for the rest of that request, so the retry immediately lands on a different fixed exit IP.
- Attribution clean → the node is quarantined (default 24h), and its per-node rotation webhook is POSTed (e.g. restart MicroWARP to obtain a new exit IP).
- After a settle delay the node is probed; the exit IP must have changed. A one-shot canary (a tiny streaming inference request: first SSE event within budget and thinking evidence present) decides re-admission.
- Canary pass → quarantine released and the node rejoins the pool. Fail → rotate again, up to `maxAttemptsPerQuarantine` (default 3) per quarantine cycle, then stay quarantined with a warning.
- With RSC attribution disabled or unlinked accounts, cross-account confirmation is the fallback: two distinct accounts degrading on the same node inside the window quarantine it.
- Only fixed (non-pool) nodes participate in quality quarantine and rotation; proxy-pool (rotating-endpoint) members are exempt from both, keeping only per-request exclusion. Rate guards: ≥10 minutes between rotations per node, ≤6 rotations per hour globally.

Configure each MicroWARP endpoint as its own fixed node and set its rotation webhook (token-checked POST endpoint that restarts the tunnel service) in the node editor; see EXIT-IP-GUARD.md for a complete rotate-server deployment guide.
### Request audits



Every inference request lands in the audit ledger (request_audits) with per-attempt
diagnostics (request_audit_attempts), including quality-guard retries (stages
`quality_hold` for withheld attempts and `quality_idle` for empty/evidence/created-timeout aborts).
Two observability columns answer "how much did the client actually receive":

- `deliveredEvents / deliveredBytes` - SSE data events forwarded and cumulative
  bytes written to the client (non-streaming: body bytes). A 200-with-error-code
  row now states exactly what reached the client; both fields are exposed in the
  admin request-audits API and the audits page performance summary.

Retention: `audit.retention` (default 0 = keep forever; non-zero 24h-8760h)
deletes aged audit rows and their attempt details in hourly batched sweeps
(500 rows/batch, 30s budget per sweep). The task starts only when retention is
non-zero - the default is byte-identical to upstream behavior. Changes require a
process restart. Independently, runtime setting `audit.retentionDays` (default
7; 0 disables) purges rows older than that many days — when both are active the
effective window is the shorter of the two, and day-scale purges above the
duration window log `audit_retention_days_purged` on every deletion sweep.


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

On `SIGTERM`/`SIGINT` the listener closes immediately — new connections are refused — while in-flight requests get a **15 s drain window**. Requests still running at the deadline (long SSE streams, video jobs) are cut, a `server_shutdown_drain_timeout` WARN is logged, and the process still exits with code **0**: an operator-initiated stop is a normal outcome and must not pollute failure-rate statistics. A non-zero exit always indicates a real failure.

After the drain, the audit ledger gets up to 10 s to flush queued records, then the database closes. The worst case (~26 s) fits the `stop_grace_period: 30s` in `docker-compose.yml`. A second `SIGTERM` during the drain is ignored; orchestrators escalate to `SIGKILL` after the grace period.

Shutdown-related logs:

- `server_started` / `server_stopped` (with `uptime_ms`, `drain_ms`) — the pairing distinguishes a clean stop from a crash that died silently.
- `server_shutdown_drain_timeout` — the drain deadline was hit and long streams were cut by design.

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

- `audit.ledgerMode`: `observe` reports ledger faults; `enforce` can pause new inference to protect billing integrity.
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

## Upstream Shape Survey Tooling

```bash
GROK2API_ADMIN_PASSWORD=... sh scripts/survey_report.sh 8003
python3 scripts/survey_harvest.py upstream-traces/unique
cd backend && GROK2API_TRACE_REPLAY_DIR=$PWD/../upstream-traces/unique go test ./internal/application/gateway/ -run TestCorpusReplay -v
python3 scripts/trace_census.py && python3 scripts/trace_census_timeline.py
```

Capture side is `internal/pkg/upstreamtrace` (env-gated, zero cost by default).

## Development

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
