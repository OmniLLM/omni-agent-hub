# Omni Agent Hub — Client Integration Guide

## Overview

The hub exposes a **single A2A endpoint** (`POST /`) that clients talk to using JSON-RPC 2.0. The hub transparently routes requests to the correct upstream agent based on skill ID, @mention, or context stickiness — clients never need to know which upstream is handling their request.

```
┌────────┐      JSON-RPC       ┌──────────────┐       JSON-RPC       ┌───────────────┐
│ Client ├─────────────────────► Omni Agent   ├──────────────────────► Upstream A    │
│        │  POST /             │   Hub        │  POST /              │ (omnilauncher)│
│        │  Bearer client-key  │ :8222        │  Bearer upstream-key └───────────────┘
│        │                     │              │
│        │                     │              ├──────────────────────► Upstream B    │
│        │                     │              │                      │ (research)    │
└────────┘                     └──────────────┘                      └───────────────┘
```

## 1. Discovery — Get the Composite Agent Card

**No auth required.** This is how your client discovers what skills are available.

```bash
GET http://localhost:8222/.well-known/agent-card.json
```

Response:
```json
{
  "name": "Omni A2A Hub",
  "url": "http://localhost:8222",
  "capabilities": { "streaming": false, "pushNotifications": false },
  "authentication": { "schemes": ["bearer"] },
  "skills": [
    { "id": "omnilauncher.plugin:tool:shell_exec", "name": "shell_exec", ... },
    { "id": "omnilauncher.skill:aws",              "name": "aws", ... },
    { "id": "research.search",                     "name": "Search", ... }
  ]
}
```

**Key detail:** Every skill ID is **namespaced** as `<upstream-name>.<original-skill-id>`. When you send a request with `skillId`, use the full namespaced form.

---

## 2. Authentication

Every `POST /` request requires a bearer token:

```
Authorization: Bearer <api_key>
```
or:
```
X-API-Key: <api_key>
```

The key is the `server.api_key` value from the hub's config.

---

## 3. Sending a Message (Unary)

### Method: `message/send`

This is the primary way to talk to an upstream agent.

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
      {
        "artifactId": "...",
        "name": "response",
        "parts": [{ "text": "total 48\ndrwxrwxrwt 12 root root ..." }]
      }
    ]
  }
}
```

### Important: The `id` in the result is a **hub-generated task ID** — it is NOT the upstream's internal task ID. Always use this ID for `tasks/get` and `tasks/cancel`.

---

## 4. Three Ways to Route a Request

The hub resolves which upstream to use in this priority order:

### 4a. By Skill ID (recommended)

Set `params.skillId` to the namespaced skill from the composite card:

```json
{
  "params": {
    "skillId": "omnilauncher.plugin:tool:web_search",
    "message": { "role": "user", "parts": [{ "text": "search for Go 1.25 release notes" }] }
  }
}
```

### 4b. By @mention

Prefix your message text with `@upstream-name`:

```json
{
  "params": {
    "message": { "role": "user", "parts": [{ "text": "@omnilauncher what time is it?" }] }
  }
}
```

The hub strips the `@omnilauncher ` prefix before forwarding.

### 4c. By Context Stickiness (automatic)

If you include a `contextId` that was used in a previous non-terminal task, the hub automatically routes to the **same upstream** — even if you don't specify `skillId` or `@mention`. This is critical for multi-turn conversations.

```json
// Turn 1 — routed to omnilauncher via skillId
{ "params": { "skillId": "omnilauncher.skill:aws", "contextId": "ctx-42", "message": { ... } } }

// Turn 2 — no skillId needed; contextId sticks to omnilauncher
{ "params": { "contextId": "ctx-42", "message": { "role": "user", "parts": [{ "text": "now show me the S3 buckets" }] } } }
```

---

## 5. Multi-Turn Conversations (`input-required`)

Some upstream agents support multi-turn workflows. The flow:

```
Client                          Hub                         Upstream
  │                              │                              │
  │── message/send (ctx-42) ────►│── message/send ─────────────►│
  │                              │                              │
  │◄── result: state=input-required ◄──────────────────────────│
  │                              │                              │
  │── message/send (ctx-42) ────►│── message/send ─────────────►│  (same upstream!)
  │                              │                              │
  │◄── result: state=completed ◄───────────────────────────────│
```

**Rules for multi-turn:**
1. Always include the same `contextId` across turns.
2. You'll get back the **same hub task ID** across turns of the same context.
3. The hub guarantees the follow-up lands on the same upstream — even if that upstream is temporarily unhealthy (you'll get a clean error, never a silent switch).

---

## 6. Streaming (SSE)

### Method: `message/sendSubscribe`

Same params as `message/send`, but the response upgrades to Server-Sent Events:

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

**Client-side handling:**
- Read events until you see `"final": true` or `"state"` is `completed`/`failed`/`canceled`.
- If the upstream disconnects abnormally, the hub synthesizes a `{"state":"failed"}` terminal event — your client will never get a silent hang.
- Every event's `id` field is the hub task ID (already rewritten).

---

## 7. Retrieving a Task

### Method: `tasks/get`

```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "method": "tasks/get",
  "params": { "id": "hub-task-uuid-xxxx" }
}
```

- For **terminal** tasks (completed/failed/canceled), the hub returns the cached snapshot immediately — no upstream call.
- For **active** tasks, the hub forwards `tasks/get` to the owning upstream and returns the latest state.

---

## 8. Canceling a Task

### Method: `tasks/cancel`

```json
{
  "jsonrpc": "2.0",
  "id": 3,
  "method": "tasks/cancel",
  "params": { "id": "hub-task-uuid-xxxx" }
}
```

The hub forwards the cancel to the upstream. If the upstream doesn't support cancellation, you'll get the upstream's error passed through.

---

## 9. Health & Monitoring

```bash
# Health (no auth)
GET /health
→ {"status":"ok","upstreams":{"total":2,"healthy":2}}

# Prometheus metrics (no auth)
GET /metrics
→ omni_a2a_upstream_healthy{upstream="omnilauncher"} 1
  omni_a2a_upstream_consecutive_failures{upstream="omnilauncher"} 0
  omni_a2a_tasks_active 3
```

---

## 10. Error Codes

All errors are standard JSON-RPC:

```json
{ "jsonrpc": "2.0", "id": 1, "error": { "code": -32011, "message": "No route", "data": "..." } }
```

| Code | Meaning | What to do |
|---|---|---|
| `-32700` | Parse error | Fix your JSON |
| `-32600` | Invalid request | Ensure `jsonrpc: "2.0"` |
| `-32601` | Method not found | Check method name (only `message/send`, `message/sendSubscribe`, `tasks/get`, `tasks/cancel`) |
| `-32602` | Invalid params | Check param structure |
| `-32001` | Task not found | The hub task ID doesn't exist |
| `-32010` | Upstream unavailable | Circuit breaker is open — the upstream had 3+ consecutive failures. Retry later. |
| `-32011` | No route | The hub couldn't match your request to any upstream. Set `skillId` or use `@mention`. |
| `-32002` | Upstream HTTP error | The upstream returned a 5xx or network error |
| `-32003` | Invalid upstream response | The upstream returned non-JSON-RPC |

---

## 11. Client Implementation Checklist

```
□ Fetch /.well-known/agent-card.json on startup to discover skills
□ Store the skill list; let users pick skills or route by convention
□ Set Authorization: Bearer <key> on every POST /
□ Always include contextId for conversations that may span multiple turns
□ Use the hub task ID (from result.id) for tasks/get and tasks/cancel — never cache upstream IDs
□ Handle state=input-required by prompting the user and sending a follow-up with the same contextId
□ For streaming: read SSE events until final=true; handle state=failed as terminal
□ On -32010 (breaker open), back off and retry in a few seconds
□ On -32011 (no route), surface to the user that no upstream handles this request
□ Periodically re-fetch the agent card to pick up new upstreams or skill changes
```

---

## 12. Compatibility: Legacy `/message:send` Endpoint

If your client previously used the path-style `POST /message:send` endpoint, it still works:

```json
POST /message:send
{
  "messages": [{ "role": "user", "parts": [{ "text": "@omnilauncher hello" }] }],
  "tool": "omnilauncher.plugin:tool:shell_exec"
}
```

This is a compatibility shim — it internally converts to `message/send`. **New clients should use the JSON-RPC endpoint at `POST /`.**

---

## 13. REST Binding: `/a2a/v1`

Clients that prefer path-style HTTP can use the REST binding. It uses the same
A2A params and hub task IDs as JSON-RPC, but omits the JSON-RPC envelope:

```bash
# Send a message: raw SendMessageParams in, raw Task out
curl -X POST http://localhost:8222/a2a/v1/message:send \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <api_key>" \
  -d '{
    "skillId": "omnilauncher.plugin:tool:shell_exec",
    "contextId": "ctx-42",
    "message": { "role": "user", "parts": [{ "text": "ls -la" }] }
  }'

# Stream a message: raw SendMessageParams in, raw A2A SSE events out
curl -N -X POST http://localhost:8222/a2a/v1/message:stream \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <api_key>" \
  -d '{
    "skillId": "omnilauncher.plugin:tool:shell_exec",
    "message": { "role": "user", "parts": [{ "text": "ls -la" }] }
  }'

# Get task status
curl -H "Authorization: Bearer <api_key>" \
  http://localhost:8222/a2a/v1/tasks/<hub-task-id>

# Cancel a task
curl -X POST -H "Authorization: Bearer <api_key>" \
  http://localhost:8222/a2a/v1/tasks/<hub-task-id>:cancel
```

REST errors are plain JSON with the same A2A error codes used by JSON-RPC:

```json
{ "error": { "code": -32011, "message": "No route" } }
```

The HTTP status reflects the error class: invalid requests return `400`, missing
tasks return `404`, unavailable upstreams return `503`, and upstream/routing
failures return `502`.
