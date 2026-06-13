# Admin API reference

The admin API runs on its own port (default 9090) and is the single
control surface of the engine. Paths are versioned under `/api/v1`;
breaking changes after a release are not allowed.

Authentication: if `server.admin.token` is set, every request must carry
`Authorization: Bearer <token>`. Without a token there is no auth (the
local-development default). All responses are JSON.

## Health

`GET /api/v1/health`

```json
{"status": "ok", "version": "1.0.0", "commit": "abc1234",
 "uptime_seconds": 42, "config_version": 3}
```

## Scenarios

`GET /api/v1/scenarios` - scenarios of the currently loaded config.

```json
{"scenarios": [{
  "id": "payment.chargeback",
  "description": "Payment succeeds, then a chargeback arrives",
  "states": ["chargeback", "new", "processing", "succeeded"],
  "initial": "new",
  "session_key": "body:$.order_id",
  "ttl": "30m0s",
  "config_version": 3
}]}
```

## Sessions

`GET /api/v1/sessions` - active sessions.

```json
{"sessions": [{
  "id": "01JF...", "key": "ord_42", "scenario": "payment.chargeback",
  "state": "processing", "vars": {"amount": "1999"},
  "config_version": 2,
  "created": "2026-06-12T10:00:00Z", "expires": "2026-06-12T10:30:00Z"
}], "count": 1}
```

`config_version` shows which config snapshot the session is pinned to;
after a hot reload old sessions keep finishing on their original version.

`GET /api/v1/sessions/{id}` - details plus queued webhooks:

```json
{"session": {...},
 "pending_webhooks": [{
   "delivery_id": "01JF...", "target": "merchant_callback",
   "event": "payment.chargeback", "fire_at": "...", "attempts": 0
 }]}
```

`DELETE /api/v1/sessions/{id}` - reset the session and cancel its queued
webhooks. Frees the session key immediately.

`POST /api/v1/sessions/{id}/advance` - force a transition. Enter actions
of the target state run (webhooks get scheduled).

```sh
curl -X POST localhost:9090/api/v1/sessions/01JF.../advance \
  -d '{"state": "chargeback"}'
```

## Deliveries

`GET /api/v1/deliveries?session=&target=&limit=` - the delivery journal,
newest first. Each record carries the full request (headers per attempt,
body), every attempt with status code, response body (truncated to 64 KB)
and timing, and the final status: `pending`, `retrying`, `delivered`,
`failed` or `canceled`.

`GET /api/v1/deliveries/{id}` - one full record.

`POST /api/v1/deliveries/{id}/replay` - resend. The optional body
`{"body": "..."}` replaces the payload (Replay and Edit). Returns 202 with
the new delivery id. The replay is re-signed at send time.

## Trigger

`POST /api/v1/trigger` - send a one-off webhook outside any scenario:

```sh
curl -X POST localhost:9090/api/v1/trigger -d '{
  "target": "merchant_callback",
  "body": {"event": "manual.test"},
  "sign": true
}'
```

`sign` defaults to true when the target has signing configured; set
`"sign": false` to send unsigned.

## Reload

`POST /api/v1/reload` - reload the config from disk, same code path as the
fsnotify watcher. An invalid config is rejected with 422 and the full
validation error list; the server keeps running on the previous version.
Active sessions are never interrupted by a reload.
