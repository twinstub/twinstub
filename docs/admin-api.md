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
 "virtual_now": "2026-11-27T10:00:00Z", "clock_offset": "720h0m0s",
 "pending_webhooks": [{
   "delivery_id": "01JF...", "target": "merchant_callback",
   "event": "payment.chargeback", "fire_at": "...", "attempts": 0
 }]}
```

`virtual_now` and `fire_at` are in session time (see Session time below);
they equal wall-clock time unless the session clock was advanced.

`DELETE /api/v1/sessions/{id}` - reset the session and cancel its queued
webhooks. Frees the session key immediately.

`POST /api/v1/sessions/{id}/advance` - force a transition. Enter actions
of the target state run (webhooks get scheduled).

```sh
curl -X POST localhost:9090/api/v1/sessions/01JF.../advance \
  -d '{"state": "chargeback"}'
```

## Session time

Every session runs on its own clock: wall-clock time plus an offset that
starts at zero, so a session nobody advanced behaves exactly as before.

`POST /api/v1/sessions/{id}/advance-time` - move one session's clock
forward and replay everything that becomes due in that window, in order,
waiting for each delivery: the webhook, the transition its first attempt
triggers, the webhooks that transition schedules, and retries. "Payment
succeeds, a chargeback arrives 30 days later" plays out in one call. Other
sessions keep running on wall time.

```sh
curl -X POST localhost:9090/api/v1/sessions/01JF.../advance-time \
  -d '{"duration": "30d"}'
```

```json
{"virtual_now": "2026-11-27T10:00:03Z", "clock_offset": "720h0m0s",
 "fired": [
   {"delivery_id": "01JF...", "event": "payment.succeeded", "target": "merchant_callback",
    "virtual_at": "2026-10-28T10:00:03Z", "attempt": 1, "result": "delivered", "status_code": 200},
   {"delivery_id": "01JG...", "event": "payment.chargeback", "target": "merchant_callback",
    "virtual_at": "2026-11-27T10:00:03Z", "attempt": 1, "result": "delivered", "status_code": 200}
 ],
 "session": {"state": "chargeback", ...}}
```

- `duration` uses the config duration syntax (`30s`, `2h`, `30d`).
- `timeout` (optional, default `60s`, at most `10m`) bounds the call. The
  replay waits for your app to answer each delivery; if the budget runs out,
  the response carries `"partial": true` with a `reason`, and the clock stays
  at the last event that was processed.
- `result` is `delivered`, `retrying` (the next retry is scheduled in session
  time and fires in the same call if it falls inside the window), `failed`
  or `canceled`.
- Template functions (`now`, `nowUnix`, `addTime`) render session time.
- Signature timestamps stay on wall-clock time: Stripe-style verifiers
  reject old timestamps, so a signature from "30 days later" would fail.
- The session TTL stays on wall-clock time: advancing 30 days does not
  expire the session.
- `409 session_busy` if another advance on the same session is running;
  `404 session_not_found`; `400` for a bad duration or timeout.

## Deliver now

`POST /api/v1/sessions/{id}/deliver` - send a session's queued webhooks
immediately, in the order given, ignoring their schedule. This stages
races on demand: "completed arrives before pending", "the refund lands
before the capture".

```sh
curl -X POST localhost:9090/api/v1/sessions/01JF.../deliver \
  -d '{"events": ["payment.completed", "payment.pending"]}'
```

```json
{"fired": [
   {"delivery_id": "01JF...", "event": "payment.completed", "target": "merchant_callback",
    "virtual_at": "...", "attempt": 1, "result": "delivered", "status_code": 200},
   {"delivery_id": "01JG...", "event": "payment.pending", "target": "merchant_callback",
    "virtual_at": "...", "attempt": 1, "result": "delivered", "status_code": 200}
 ],
 "session": {...}}
```

- Each item is an event name or a delivery id. An event name picks the
  earliest queued delivery of that event.
- A transition triggered by an earlier item may queue a later one, so
  `["payment.succeeded", "payment.chargeback"]` works for a chained flow.
- The session clock does not move. Webhooks not named keep their schedule;
  retries of a failed attempt continue on it.
- An item that is not queued stops the call with `422 not_pending`, listing
  `pending_events` and what was already sent in `fired`. Only queued
  webhooks can be sent early; to resend one that was already delivered,
  use replay.
- `timeout` works as in advance-time. `409 session_busy`,
  `404 session_not_found` and `400` behave the same way.

## Deliveries

`GET /api/v1/deliveries?session=&target=&limit=` - the delivery journal,
newest first. Each record carries the full request (headers per attempt,
body), every attempt with status code, response body (truncated to 64 KB)
and timing, and the final status: `pending`, `retrying`, `delivered`,
`failed` or `canceled`. Attempts in a session whose clock was advanced also
carry `virtual_at`, the session time of the attempt; `at` stays wall-clock.

`GET /api/v1/deliveries/{id}` - one full record.

`POST /api/v1/deliveries/{id}/replay` - resend. Returns 202 with the new
delivery id; the replay is re-signed at send time and never moves the
session's state. Optional body:

```json
{"mode": "duplicate", "wait": true, "timeout": "30s"}
```

| `mode` | What the receiver gets |
|---|---|
| `as_is` (default) | the same body, or `body` if given (Replay and Edit), under a new delivery id |
| `duplicate` | a byte-identical repeat: same body and the same `X-TwinStub-Delivery`, `-Event` and `-Session` as the original, with a fresh signature, the way provider retries look. A duplicate of a duplicate still carries the first id. `body` is not allowed |
| `bad_signature` | a well-formed signature that does not verify (`422 signing_not_configured` for an unsigned target) |
| `no_signature` | no signature header at all |

With `"wait": true` the call waits for the first attempt (up to `timeout`,
default `60s`) and returns `200` with the whole record under `delivery`,
including the receiver's status code and response body.

Journal records made by a replay carry `mode` (`replay`, `duplicate`,
`bad_signature`, `no_signature`) and `replayed_from`; `header_id` shows the
`X-TwinStub-Delivery` value when it differs from the record id. Every
attempt carries `signature`: `valid`, `invalid` or `none`.

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
