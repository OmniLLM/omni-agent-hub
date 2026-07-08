# Omni Agent Hub

[![Go Report Card](https://goreportcard.com/badge/github.com/OmniLLM/omni-agent-hub)](https://goreportcard.com/report/github.com/OmniLLM/omni-agent-hub)
[![Go Version](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)

Omni Agent Hub is a **pure Go A2A (Agent-to-Agent) protocol hub** that aggregates multiple upstream A2A agents behind a single unified endpoint. Clients talk to one URL — the hub transparently routes requests to the correct upstream agent, translates task IDs, relays streaming events, and provides circuit-breaking health management.

The hub ships as a single binary named **`oah`** (Omni A2A Hub), which serves the HTTP surface and doubles as a full-featured admin CLI.

```
┌─────────────────┐     A2A request       ┌──────────────────┐     A2A request       ┌─────────────────┐
│                 │    POST /             │                  │    POST /             │                 │
│   Client App    │──────────────────────►│   Omni Agent     │──────────────────────►│  Upstream A     │
│                 │   Bearer client-key   │   Hub :8222      │   Bearer upstream-key  │  (omnilauncher) │
│                 │                       │                  │                        │                 │
│   .─────────.   │                       │   .───────────.  │                        │  ┌───────────┐  │
│  ( A2A SDK   )  │                       │  ( Composite   ) │                        │  │ Shell Exec │  │
│   '─────────'   │                       │  ( Agent Card  ) │                        │  └───────────┘  │
│                 │                       │   '───────────'  │                        │  ┌───────────┐  │
│                 │                       │                  │                        │  │ Web Search │  │
│                 │                       │   router ──►     ├──────────────────────►│  └───────────┘  │
│                 │                       │                  │                        └─────────────────┘
│                 │                       │   dispatch ──►   │
│                 │                       │                  │                        └─────────────────┘
│                 │                       │   registry       │                        │  Upstream B     │
│                 │                       │   ┌─────────┐    │                        │  (research)     │
│                 │                       │   │ Breaker │    │                        └─────────────────┘
│                 │                       │   └─────────┘    │
│                 │                       └──────────────────┘
```

## Features

- **🔀 Unified Endpoint** — One URL for all upstream A2A agents. Clients never need to know about individual upstreams.
- **🔌 Dual Transport** — Both **JSON-RPC 2.0** (`POST /`) and the **A2A HTTP+JSON REST binding** (`POST /a2a/v1/message:send`, `.../message:stream`, `GET /a2a/v1/tasks/{id}`) are served from the same routing core.
- **📋 Composite Agent Card** — `GET /.well-known/agent-card.json` returns a namespaced union of every healthy upstream's skills (e.g., `omnilauncher.shell_exec`, `research.search`).
- **🧭 Deterministic Routing** — Four routing strategies in priority order:
  1. **Context stickiness** — multi-turn conversations stay on the same upstream
  2. **Skill ID** — route by namespaced skill (`upstream.skill_id`)
  3. **@mention** — route by `@upstream_name` prefix in message text
  4. **Text prefix** — route by configurable text prefix
- **🔁 Streaming (SSE)** — `message/sendSubscribe` (JSON-RPC) and `message:stream` (REST) upgrade to Server-Sent Events with transparent task-ID rewriting and synthetic terminal events on upstream disconnection.
- **🛡️ Circuit Breaker** — 3 consecutive failures mark an upstream unhealthy; exponential backoff (`2^min(failures-3, 6) * 1s`) prevents cascading failures.
- **🆔 Task-ID Translation** — Hub-visible task IDs are isolated from upstream-issued IDs. Clients never see raw upstream IDs.
- **⚡ Admin API + CLI** — Add, remove, edit, refresh, test, and inspect upstreams at runtime without restarting the hub — over HTTP or via `oah` subcommands.
- **💾 SQLite Persistence** — Upstream registrations, health state, task-ID mappings, and audit logs survive restarts.
- **📊 Prometheus Metrics** — `/metrics` endpoint with upstream health, failure counts, and active task gauges.
- **🔄 Daemon Management** — PID-file based start/stop/restart/status, with optional systemd service.
- **🧪 CGO-Free** — Pure Go binary with no C toolchain requirement. Static single-binary distribution.

## Architecture

```
┌─────────────────────────────────────────────────────────────────────────┐
│                         omni-agent-hub (oah)                              │
│                                                                          │
│  ┌──────────────┐     ┌──────────────┐     ┌──────────────────────────┐ │
│  │   Transport   │────►│   Dispatch   │────►│   Store (SQLite)         │ │
│  │   (HTTP)      │     │  (Unary +    │     │  ┌────────────────────┐ │ │
│  │              │     │   Stream)    │     │  │ upstreams         │ │ │
│  │  JSON-RPC /   │     │              │     │  │ tasks             │ │ │
│  │  REST /a2a/v1 │     │  SendMessage │     │  │ task_id_map       │ │ │
│  │  /admin/*     │     │  GetTask     │     │  │ audit_log         │ │ │
│  │  /.well-known │     │  CancelTask  │     │  └────────────────────┘ │ │
│  │  /health      │     │  Subscribe   │     └──────────────────────────┘ │
│  │  /metrics     │     └──────┬───────┘                                   │
│  └──────┬───────┘            │                                           │
│         │                    ▼                                           │
│         ▼             ┌──────────────┐                                   │
│  ┌──────────────┐     │   Registry   │                                   │
│  │   Card       │◄────│  (upstream   │                                   │
│  │  (composite  │     │   lifecycle  │                                   │
│  │   AgentCard) │     │   + breaker) │                                   │
│  └──────────────┘     └──────┬───────┘                                   │
│                              │                                           │
│  ┌──────────────┐            │                                           │
│  │   Router     │  (pure,    │                                           │
│  │  (routing    │   no I/O)  │                                           │
│  │   logic)     │            │                                           │
│  └──────────────┘            ▼                                           │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │  Upstream A (omnilauncher)   Upstream B (research)   ...         │   │
│  └──────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────┘
```

### Internal Packages

| Package | Responsibility |
|---|---|
| `a2a` | Pure protocol types — JSON-RPC envelopes, AgentCard, Task, Message, SSE events, error codes. Zero dependencies on other hub packages. |
| `config` | YAML load/save with auto-migration from legacy shapes. Server, hub identity, storage, logging, and upstream sections. API keys auto-generated if missing. |
| `store` | Thin typed CRUD over SQLite (via `modernc.org/sqlite` — pure Go). Tables: `upstreams`, `tasks`, `task_id_map`, `audit_log`. WAL mode, single-connection serialized access. |
| `registry` | Authoritative in-memory upstream list with card cache, health state, and circuit breaker. Emits change events on a buffered channel for the card builder. Concurrency via `sync.RWMutex`. |
| `card` | Composite AgentCard builder. Subscribes to registry events via a single goroutine, debounces bursts (100ms), swaps the card atomically. Readers are lock-free via `atomic.Pointer`. |
| `router` | Pure, I/O-free request resolver. Given a `Snapshot` of upstreams and a `Request` (skill ID, text, context ID), returns a deterministic `Resolution`. Fully table-driven testable. |
| `dispatch` | Request proxying engine. Hides task-ID translation, circuit-breaker checks, SSE relay, and audit logging behind two interfaces: `Unary` and `Stream`. |
| `transport` | HTTP handlers only — no business logic. Serves the JSON-RPC surface, the REST (A2A HTTP+JSON) binding, and the admin API, across three auth tiers: public, client, admin. |
| `logging` | Structured slog setup with dual output (stdout + rotating file). JSON format by default, text toggle. |
| `tail` | Log tailing helper (last N lines + follow) backing `oah logs`. |
| `cli` | Cobra command tree: `serve`, daemon control, `health`, `skills`, `audit`, `task`, `message`, `upstream`, `config`, `version`. |

## Quick Start

### Prerequisites

- Go 1.25+ (no C compiler required)

### Install & Run

```bash
# Clone and build (produces the ./oah binary)
git clone https://github.com/OmniLLM/omni-agent-hub.git
cd omni-agent-hub
make build

# Or install to $HOME/go/bin (also symlinks the legacy `omni-agent-hub` name)
make install

# Start as foreground server
./oah serve

# Or start as background daemon
./oah start

# Check status
./oah status

# View logs
./oah logs -f
```

The server starts on `0.0.0.0:8222` by default. A quick-start wrapper is also available:

```bash
./start.sh          # builds if needed, then starts the daemon
./start.sh serve    # forwards any subcommand to oah
```

### Systemd Service

```bash
make install-service
sudo systemctl enable --now omni-agent-hub.service
```

## Configuration

Configuration is read from `~/.config/omni-agent-hub/config.yaml`. Generate one with the interactive wizard (`oah config init`), or let the hub auto-generate a default on first run — but you must set `api_key` and `admin_key` before use (if left empty, they are auto-generated and logged at WARN level on startup).

```yaml
server:
  host: "0.0.0.0"
  port: 8222
  public_url: "http://localhost:8222"          # advertised in composite AgentCard
  api_key: "ad6450af..."                       # client bearer, required
  admin_key: "CHANGE_ME_admin_secret"          # admin bearer, required

hub:
  name: "Omni A2A Hub"
  description: "Aggregator for local and remote A2A agents."

storage:
  path: "~/.omni-agent-hub/state.db"           # SQLite file
  audit_retention: 10000                       # rows kept in audit_log

logging:
  file: "~/.omni-agent-hub/logs/server.log"
  level: "info"                                 # debug | info | warn | error
  format: "json"                                # json | text

upstream:
  - name: "omnilauncher"
    base_url: "http://localhost:1423"
    prefix: "@omnilauncher"                     # optional routing prefix
    auth:
      scheme: "bearer"                          # bearer | none
      token: "70020642d1f2..."                  # upstream's auth token
    enabled: true
```

### CLI Override Flags

Global flags apply to every subcommand:

```bash
oah serve --config /path/to/config.yaml   # override config path
oah serve --host 127.0.0.1 --port 9000    # override bind address and port
oah serve --log-file /var/log/oah.log     # override log file
oah health --no-color                     # disable ANSI color output
```

### Configuration Details

- **`api_key`** — Client-facing bearer key. Required. Sent by clients as `Authorization: Bearer <key>` or `X-API-Key: <key>`. Public endpoints (`/.well-known/*`, `/health`, `/metrics`) do not require auth.
- **`admin_key`** — Admin API key. Required and distinct from `api_key`. If left empty, one is auto-generated and logged at WARN level on startup.
- **`public_url`** — The URL advertised in the composite AgentCard. Must be reachable by clients. Required.
- **Storage** — SQLite database at `storage.path` (default: `~/.omni-agent-hub/state.db`). WAL mode. Single-connection for serialized access. Audit log is capped at `audit_retention` rows on startup.

## HTTP Endpoints

| Tier | Method & Path | Purpose |
|---|---|---|
| Public | `GET /.well-known/agent-card.json` | Composite AgentCard (also `GET /.well-known/agent.json`) |
| Public | `GET /health` | Liveness + upstream health summary |
| Public | `GET /metrics` | Prometheus metrics |
| Public | `OPTIONS /` | CORS preflight |
| Client | `POST /` | JSON-RPC 2.0: `message/send`, `message/sendSubscribe`, `tasks/get`, `tasks/cancel` |
| Client | `POST /a2a/v1/message:send` | REST binding — bare `MessageSendParams` in, raw `Task` out |
| Client | `POST /a2a/v1/message:stream` | REST binding — SSE stream of raw A2A events |
| Client | `GET /a2a/v1/tasks/{id}` | REST binding — fetch task by hub task ID |
| Client | `POST /a2a/v1/tasks/{id}:cancel` | REST binding — cancel task |
| Client | `POST /message:send` | Legacy path-style compat shim (→ `message/send`) |
| Client | `POST /message:stream` | Root alias for `message:stream` |
| Admin | `GET`/`POST /admin/upstreams` | List / add upstreams |
| Admin | `GET`/`DELETE /admin/upstreams/{id}` | Get / remove one upstream |
| Admin | `POST /admin/upstreams/{id}/refresh` | Re-fetch one upstream's card |
| Admin | `POST /admin/upstreams/{id}/test` | Probe one upstream (no state change) |
| Admin | `POST /admin/refresh` | Re-fetch all upstream cards |
| Admin | `GET /admin/skills` | Flat skill index (debugging) |
| Admin | `GET /admin/health` | Detailed per-upstream health |
| Admin | `GET /admin/tasks` · `GET /admin/tasks/{id}` | List / inspect tasks |
| Admin | `POST /admin/tasks/{id}/cancel` | Cancel a task |
| Admin | `GET /admin/audit` | Dispatch audit log |
| Admin | `POST /admin/messages` | Send a message to a chosen upstream |
| Admin | `GET /admin/version` | Running hub version |

## Usage

### 1. Discovery — Get the Composite Agent Card

```bash
GET http://localhost:8222/.well-known/agent-card.json
```

Response includes every healthy upstream's skills, namespaced as `<upstream-name>.<skill-id>`:

```json
{
  "name": "Omni A2A Hub",
  "url": "http://localhost:8222",
  "capabilities": { "streaming": true, "pushNotifications": false },
  "authentication": { "schemes": ["bearer"] },
  "skills": [
    { "id": "omnilauncher.plugin:tool:shell_exec", "name": "shell_exec", "description": "Execute shell commands" },
    { "id": "omnilauncher.skill:aws",              "name": "AWS",        "description": "Manage AWS resources" },
    { "id": "research.search",                     "name": "Search",     "description": "Web search" }
  ]
}
```

> **Key detail:** Always use the full namespaced skill ID (e.g., `omnilauncher.plugin:tool:shell_exec`) when sending requests.

### 2. Authentication

Every `POST /` request requires a bearer token:

```
Authorization: Bearer <api_key>
```

or:

```
X-API-Key: <api_key>
```

Use the `server.api_key` value from the config. The admin API (`/admin/*`) uses `server.admin_key` — it is intentionally distinct so client access doesn't grant registry mutation.

### 3. Send a Message (JSON-RPC, Unary)

```bash
curl -X POST http://localhost:8222/ \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <api_key>" \
  -d '{
    "jsonrpc": "2.0",
    "id": 1,
    "method": "message/send",
    "params": {
      "skillId": "omnilauncher.plugin:tool:shell_exec",
      "contextId": "my-conversation-1",
      "message": {
        "messageId": "msg-001",
        "role": "user",
        "parts": [{ "text": "ls -la /tmp" }]
      }
    }
  }'
```

Response:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "id": "hub-task-uuid-xxxx",
    "contextId": "my-conversation-1",
    "status": { "state": "completed" },
    "artifacts": [
      { "artifactId": "...", "name": "response", "parts": [{ "text": "total 48\ndrwxrwxrwt ..." }] }
    ]
  }
}
```

> **Important:** The `id` in the result is a **hub-generated task ID**. Always use this ID for `tasks/get` and `tasks/cancel`. Never cache or use upstream-issued IDs — they are invisible to clients.

### 4. Send a Message (REST / A2A HTTP+JSON)

For clients using the A2A HTTP+JSON binding, POST a bare `MessageSendParams` body (no JSON-RPC envelope) and get a raw `Task` back:

```bash
curl -X POST http://localhost:8222/a2a/v1/message:send \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <api_key>" \
  -d '{
    "skillId": "omnilauncher.plugin:tool:shell_exec",
    "message": { "role": "user", "parts": [{ "text": "ls -la" }] }
  }'
```

The response is the raw `Task` JSON (HTTP 200). Errors are returned as plain-HTTP JSON (`{"error":{"code":...,"message":"..."}}`) with an appropriate status code instead of a JSON-RPC envelope. Use `GET /a2a/v1/tasks/{id}` and `POST /a2a/v1/tasks/{id}:cancel` for task management, and `POST /a2a/v1/message:stream` for SSE streaming.

### 5. Routing Strategies

The hub resolves which upstream handles a request in this priority order:

| Priority | Strategy | How |
|---|---|---|
| 1 | **Context Stickiness** | If `contextId` matches a non-terminal task, route to the same upstream automatically. |
| 2 | **Skill ID** | Set `params.skillId` to the full namespaced skill from the composite card. |
| 3 | **@mention** | Prefix message text with `@upstream_name` (e.g., `@omnilauncher what time is it?`). The hub strips the mention before forwarding. |
| 4 | **Text Prefix** | If no other strategy matches, upstream prefix patterns are checked. |

#### Context Stickiness — Essential for Multi-Turn

```json
// Turn 1 — routed to omnilauncher via skillId
{ "params": { "skillId": "omnilauncher.skill:aws", "contextId": "ctx-42", "message": { ... } } }

// Turn 2 — no skillId needed; contextId sticks to omnilauncher
{ "params": { "contextId": "ctx-42", "message": { "role": "user", "parts": [{ "text": "now show S3 buckets" }] } } }
```

This guarantees that multi-turn conversations (tasks with `input-required` state) always land on the same upstream, even if that upstream becomes unhealthy — you'll get a clean error rather than a silent upstream switch.

### 6. Streaming (SSE)

Use `message/sendSubscribe` (JSON-RPC) or `POST /a2a/v1/message:stream` (REST) for streaming responses:

```bash
curl -N -X POST http://localhost:8222/ \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <api_key>" \
  -d '{
    "jsonrpc": "2.0",
    "id": 1,
    "method": "message/sendSubscribe",
    "params": {
      "skillId": "omnilauncher.plugin:tool:shell_exec",
      "message": { "role": "user", "parts": [{ "text": "ls -la" }] }
    }
  }'
```

Response (SSE stream):

```
data: {"id":"hub-task-uuid","status":{"state":"working"},"final":false}

data: {"id":"hub-task-uuid","status":{"state":"working","message":{...}},"final":false}

data: {"id":"hub-task-uuid","status":{"state":"completed"},"final":true}
```

**Stream guarantees:**

- Every event's `id` field is the hub task ID (already rewritten from upstream IDs).
- Read events until `final: true` or state is `completed`/`failed`/`canceled`.
- If the upstream disconnects abnormally, the hub synthesizes a `{"state":"failed"}` terminal event — clients will never get a silent hang.
- The hub rewrites `task_id_map` on the first event so `tasks/get` works mid-stream.

### 7. Task Management

```bash
# Get task status (cached for terminal tasks, forwards for active)
curl -X POST http://localhost:8222/ \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <api_key>" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tasks/get","params":{"id":"hub-task-uuid-xxxx"}}'

# Cancel a task
curl -X POST http://localhost:8222/ \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <api_key>" \
  -d '{"jsonrpc":"2.0","id":3,"method":"tasks/cancel","params":{"id":"hub-task-uuid-xxxx"}}'
```

### 8. Health & Monitoring

```bash
# Health check (no auth required)
GET /health
→ {"status":"ok","upstreams":{"total":2,"healthy":2}}

# Prometheus metrics (no auth required)
GET /metrics
→ # HELP omni_a2a_upstream_healthy 1 if upstream is healthy.
  # TYPE omni_a2a_upstream_healthy gauge
  omni_a2a_upstream_healthy{upstream="omnilauncher"} 1
  omni_a2a_upstream_consecutive_failures{upstream="omnilauncher"} 0
  # HELP omni_a2a_tasks_active count of non-terminal tasks.
  # TYPE omni_a2a_tasks_active gauge
  omni_a2a_tasks_active 3
```

### 9. Admin API

Manage upstream agents at runtime without restarting the hub. All admin endpoints require `Authorization: Bearer <admin_key>`. (Most operations also have first-class `oah` subcommands — see below.)

```bash
# List upstreams
curl -H "Authorization: Bearer <admin_key>" http://localhost:8222/admin/upstreams

# Add a new upstream
curl -X POST http://localhost:8222/admin/upstreams \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <admin_key>" \
  -d '{"name":"research","base_url":"http://localhost:8003","prefix":"@research","auth":{"scheme":"none"}}'

# Remove an upstream (by id)
curl -X DELETE -H "Authorization: Bearer <admin_key>" http://localhost:8222/admin/upstreams/<id>

# Refresh all upstream cards
curl -X POST -H "Authorization: Bearer <admin_key>" http://localhost:8222/admin/refresh

# Probe one upstream without touching its cached card or health state
curl -X POST -H "Authorization: Bearer <admin_key>" http://localhost:8222/admin/upstreams/<id>/test

# View flat skill index (debugging)
curl -H "Authorization: Bearer <admin_key>" http://localhost:8222/admin/skills

# Inspect the dispatch audit log
curl -H "Authorization: Bearer <admin_key>" http://localhost:8222/admin/audit
```

## CLI (`oah`)

The `oah` binary is both the server and the admin CLI. CLI commands that talk to a running hub read `server.public_url` and `server.admin_key` from the config.

```bash
# ── Server / daemon ────────────────────────────────────────────
oah serve [--host 0.0.0.0] [--port 8222]   # foreground server
oah start                                  # background daemon
oah stop [--force]                         # graceful stop (SIGTERM) / force kill (SIGKILL)
oah restart                                # stop + start
oah status                                 # show daemon status
oah logs [-f] [-n 200]                     # show / follow the server log

# ── Observability ──────────────────────────────────────────────
oah health [--json]                        # upstream health dashboard
oah skills [--upstream X] [--match str]    # skills across upstreams
oah audit [--limit N] [--event send]       # dispatch audit log
oah task list [--recent] [--state ...]     # list tasks (alias: tasks)
oah task inspect [task-id]                 # detailed task info (interactive if omitted)
oah task cancel <task-id>                  # cancel an active task
oah version [--remote] [--json]            # CLI (and, with --remote, running hub) version

# ── Upstream management (via admin API) ────────────────────────
oah upstream list                          # alias: up
oah upstream add <name> --url http://...   # non-interactive; omit flags for wizard
oah upstream remove <id>
oah upstream refresh
oah upstream inspect [name-or-id]          # full config, health, and card
oah upstream test [name-or-id]             # probe connectivity + latency (no state change)
oah upstream edit                          # interactively edit an upstream

# ── Messaging ──────────────────────────────────────────────────
oah message send [--upstream X] [--skill Y] [--text "..."]   # alias: msg; interactive if no flags

# ── Configuration ──────────────────────────────────────────────
oah config init                            # interactive wizard to create config.yaml
oah config show                            # print resolved config with defaults applied
oah config migrate                         # rewrite config.yaml in the current shape
```

## Error Codes

All JSON-RPC errors use standard JSON-RPC 2.0 error objects; the REST binding returns the same numeric `code` inside a plain-HTTP `{"error":{...}}` body.

```json
{ "jsonrpc": "2.0", "id": 1, "error": { "code": -32011, "message": "No route", "data": "..." } }
```

| Code | Meaning | What to Do |
|---|---|---|
| `-32700` | Parse error | Fix your JSON payload |
| `-32600` | Invalid request | Ensure `jsonrpc: "2.0"` is set |
| `-32601` | Method not found | Check method name (`message/send`, `message/sendSubscribe`, `tasks/get`, `tasks/cancel`) |
| `-32602` | Invalid params | Check parameter structure matches the spec |
| `-32603` | Internal error | Unexpected hub-side error |
| `-32000` | Generic error | Unclassified error; inspect the message |
| `-32001` | Task not found | The hub task ID doesn't exist or is expired |
| `-32002` | Upstream HTTP error | Upstream returned 5xx or had a network error. Not a client issue. |
| `-32003` | Invalid upstream response | Upstream returned non-JSON-RPC content. Contact upstream operator. |
| `-32010` | Upstream unavailable | Circuit breaker open — upstream has 3+ consecutive failures. Retry later. |
| `-32011` | No route | Hub couldn't match to any upstream. Set `skillId` or use `@mention`. |

## Circuit Breaker

The hub implements a passive circuit breaker that activates after 3 consecutive failures:

- **Healthy** → normal operation, skills visible in composite card
- **Unhealthy** → after 3 consecutive failures, upstream skills removed from composite card, requests fail-fast with `unavailable` (code `-32010`) if inside the backoff window
- **Backoff** — `2^min(failures-3, 6)` seconds (1s → 64s ceiling)
- **Recovery** — a single success resets the counter to 0 and flips back to `healthy`

**What counts as a failure:** Network error, timeout, HTTP 5xx, HTTP 502/503/504, malformed (non-JSON-RPC) 200 response.

**What does NOT count:** HTTP 4xx (client-caused errors). These are passed through to the client and don't affect breaker state.

> **Note:** There is no background health pinger. Health state is derived purely from real request outcomes. Card refreshes (`.well-known` fetches) do NOT reset the breaker — they only update the card cache.

## Data Model (SQLite)

The hub stores all state in a single SQLite file (`~/.omni-agent-hub/state.db` by default).

| Table | Purpose |
|---|---|
| `upstreams` | Persistent upstream registry with health state, auth config, and cached AgentCard JSON. Config entries on startup overlay DB rows; DB retains health state. |
| `tasks` | Hub-visible task rows. One row per `hub_task_id`. Terminal states cached for `tasks/get` replay. |
| `task_id_map` | Maps hub-visible task IDs to upstream-issued IDs. The hub never exposes upstream IDs to clients. |
| `audit_log` | Append-only dispatch event log for debugging. Capped at `audit_retention` rows on startup. |

Migrations are managed inline via `PRAGMA user_version`. The schema is embedded in the binary (Go `embed` directive).

## Client Integration

Two guides cover integration in depth:

- **[Client Integration Guide](docs/client-integration-guide.md)** — for plain HTTP clients (UIs, CLI wrappers, scripted callers). Wire protocol, auth, routing, streaming.
- **[Agent Integration Guide](docs/agent-integration-guide.md)** — for AI agents (LLM tool-loop clients) that use the hub as a fan-out backend for tools and skills.

### Client Checklist

- □ Fetch `/.well-known/agent-card.json` on startup to discover available skills
- □ Use namespaced skill IDs (e.g., `omnilauncher.plugin:tool:shell_exec`)
- □ Send `Authorization: Bearer <api_key>` on every request
- □ Always include `contextId` for conversations that may span multiple turns
- □ Use the hub task ID (from `result.id`) for `tasks/get` and `tasks/cancel`
- □ Handle `state: "input-required"` by sending a follow-up with the same `contextId`
- □ For streaming: consume SSE events until `final: true`; handle `state: "failed"` as terminal
- □ On `-32010` (breaker open), back off and retry after a few seconds
- □ On `-32011` (no route), surface that no upstream handles this request
- □ Periodically re-fetch the agent card to discover new upstreams or skill changes

## Development

```bash
# Build (produces ./oah)
make build

# Run in development mode (go run, foreground serve)
make run-dev

# Run all tests (unit + integration)
make test

# Format code
make fmt

# Run linter
make lint

# Clean build artifacts
make clean
```

Handy `make` targets wrap common `oah` commands against a locally built binary: `health`, `skills`, `audit`, `tasks`, `version`, `config-init`, `upstream-list`, `upstream-refresh`, `upstream-inspect`, `upstream-test`, and `message-send`.

### Project Structure

```
cmd/omni-agent-hub/          # Entry point — cobra CLI wiring
  main.go                    # Just calls cli.NewRootCmd().Execute()

internal/
  a2a/                       # Protocol types (JSON-RPC, AgentCard, Task, Message, errors)
  card/                      # Composite AgentCard builder (atomic pointer + registry events)
  cli/                       # Cobra commands: serve, daemon, health, skills, audit, task, message, upstream, config, version
  config/                    # YAML config loader with auto-migration
  dispatch/                  # Request proxy: Unary (Send/Get/Cancel) and Stream (SSE relay)
  integration/               # End-to-end tests: real hub vs. fake upstreams
  logging/                   # Structured slog setup (stdout + file, JSON/text)
  registry/                  # Upstream lifecycle, card cache, circuit breaker
  router/                    # Pure request routing logic (no I/O)
  store/                     # SQLite persistence (upstreams, tasks, task_id_map, audit_log)
  tail/                      # Log tailing helper (last N lines + follow)
  transport/                 # HTTP handlers: JSON-RPC, REST binding, admin API, middleware

docs/
  client-integration-guide.md  # Client integration with examples
  agent-integration-guide.md    # AI-agent integration design guide
  superpowers/specs/            # Architecture design documents
```

### Testing

```bash
# Run everything
go test ./...

# Or per package
go test ./internal/a2a/...
go test ./internal/config/...
go test ./internal/store/...
go test ./internal/registry/...
go test ./internal/card/...
go test ./internal/router/...
go test ./internal/dispatch/...
go test ./internal/transport/...

# Integration tests (boots real hub against fake upstreams)
go test ./internal/integration/...
```

Integration tests boot a real hub against `httptest.Server` fake upstreams and verify:
- Multi-turn task routing through `input-required` states
- SSE event relay with correct ID rewriting
- Circuit breaker behavior with failing upstreams
- Admin API operations

## Upgrading from Legacy Config

If you're upgrading from a pre-hub version of omni-agent-hub:

1. The `agent:` block (local Hermes executor) has been removed. Run Hermes as a separate A2A server and register it as an upstream.
2. `upstream[].token` is now nested under `upstream[].auth.token`.
3. New required fields: `server.admin_key`, `server.public_url`.
4. Run `oah config migrate` to rewrite your config.yaml in the current shape.
5. Legacy fields are auto-detected and migrated on load with WARN-level log messages.

The binary is now named `oah`; `make install` also creates an `omni-agent-hub` symlink for backward compatibility.

## Design Decisions

- **CGO-free** — Pure Go SQLite via `modernc.org/sqlite` eliminates C toolchain dependencies and cross-compilation headaches.
- **Single connection** — SQLite operates with `MaxOpenConns=1` to avoid "database is locked" errors in a low-QPS service.
- **Handler thinness** — Transport layer handlers do exactly three things: parse request, call one method on a business-logic package, serialize response. No business logic in HTTP handlers.
- **Router purity** — The router is a deterministic function of its inputs. No I/O, no locks, making it trivially testable via table-driven tests.
- **Event-driven card** — The composite card rebuilds on registry change events with debouncing, never on every read. Readers hit an `atomic.Pointer` — lock-free.
- **No active health pinger** — Health comes from real traffic. This avoids false positives during quiet periods and keeps the architecture simple.
- **Context stickiness over intent** — A multi-turn conversation stays on the same upstream even if that upstream becomes unhealthy. The client gets a clean error rather than a silent upstream switch.

## Non-Goals

- **No local execution** — The hub is a pure aggregator. Agents run as separate processes.
- **No load balancing** — No distributing requests across multiple agents with the same skill.
- **No push notifications** — The hub does not originate pushes; it only advertises the capability if an upstream supports it.
- **No multi-tenancy** — Single client API key today.
- **No active health pinging** — No background goroutine probes upstream health.

## Roadmap / Future Work

- **Secret storage** — Move upstream auth tokens from SQLite to OS keyring or encrypted file store.
- **Hermes standalone A2A server** — Extract local agent execution into a separate A2A server binary.
- **Multi-tenant auth** — Per-client scoped API keys with routing rules.
- **Card-diff optimization** — Only rebuild composite card when the skill set changes, not on every health flip.

## License

MIT

## Author

[@OmniLLM](https://github.com/OmniLLM)
