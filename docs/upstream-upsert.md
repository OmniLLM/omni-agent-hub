# Upstream Upsert API

`POST /admin/upstreams/upsert` creates or updates an upstream by name.

This endpoint exists for upstream agents that can self-register on startup, such as the OmniLauncher backend. It is idempotent: repeated calls with the same `name` update the existing upstream's URL/auth/prefix rather than returning duplicate-name conflict.

## Request

Requires admin auth (`Authorization: Bearer <admin_key>` or `X-API-Key`).

```json
{
  "name": "omnilauncher",
  "base_url": "http://127.0.0.1:1423",
  "prefix": "@omnilauncher",
  "auth": {
    "scheme": "bearer",
    "token": "<upstream-a2a-token>"
  }
}
```

## Response

- `201 Created` when a new upstream was created
- `200 OK` when an existing upstream with the same name was updated

Response body is the normal upstream info JSON.
