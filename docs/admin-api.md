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
{"status": "ok", "version": "1.1.0", "commit": "abc1234",
 "uptime_seconds": 42, "config_version": 3,
 "config_path": "/home/me/project/twinstub.yaml", "public_port": 8080,
 "time_scale": 1, "seed": 42, "sessions": 2}
```

`public_port` is where your app should send its provider calls. `seed` is
present only when the engine runs with `--seed`.

## Scenarios

`GET /api/v1/scenarios` - the currently loaded config: every scenario
with its rules and webhooks as flat lists, plus stateless endpoints and
webhook targets. `?id=<scenario>` narrows it to one scenario (`404` if
unknown).

```json
{"scenarios": [{
  "id": "payment.chargeback",
  "description": "Payment succeeds, then a chargeback arrives",
  "states": ["chargeback", "new", "processing", "succeeded"],
  "initial": "new",
  "session_key": "body:$.order_id",
  "ttl": "30m0s",
  "config_version": 3,
  "rules": [
    {"state": "new", "method": "POST", "path": "/v1/payments", "goto": "processing"},
    {"state": "processing", "method": "GET", "path": "/v1/payments/{id}"}
  ],
  "webhooks": [
    {"state": "processing", "target": "merchant_callback", "event": "payment.succeeded", "after": "3s", "goto": "succeeded"},
    {"state": "succeeded", "target": "merchant_callback", "event": "payment.chargeback", "after": "30d", "goto": "chargeback"}
  ]
}],
 "endpoints": [{"id": "get_rates", "method": "GET", "path": "/v1/rates"}],
 "targets": [{"name": "merchant_callback", "url": "http://localhost:9999/webhook", "signed": true}]}
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

`POST /api/v1/sessions` - start a session without waiting for a request
from your app, optionally directly in a given state:

```sh
curl -X POST localhost:9090/api/v1/sessions \
  -d '{"scenario": "payment.chargeback", "key": "ord_42", "state": "processing"}'
```

`key` must be the value the scenario's session key selector extracts from
your app's requests (here the order id), so the app's next request joins
this session. With `state`, the session starts there and that state's
enter actions run; the initial state's do not. Returns `201` with the
session, or `200` with the existing one if the key already has a session of
the same scenario. `409 session_conflict` if the key belongs to another
scenario, `422` for an unknown scenario or state, `400` without a key.

`GET /api/v1/sessions/{id}` - details plus queued webhooks:

```json
{"session": {...},
 "virtual_now": "2026-11-27T10:00:00Z", "clock_offset": "720h0m0s",
 "history": [
   {"to": "new", "at": "...", "virtual_at": "...", "cause": "created", "detail": "POST /v1/payments"},
   {"from": "new", "to": "processing", "at": "...", "virtual_at": "...", "cause": "request", "detail": "POST /v1/payments"},
   {"from": "processing", "to": "succeeded", "at": "...", "virtual_at": "...", "cause": "webhook", "detail": "payment.succeeded"}
 ],
 "pending_webhooks": [{
   "delivery_id": "01JF...", "target": "merchant_callback",
   "event": "payment.chargeback", "fire_at": "...", "attempts": 0
 }]}
```

`virtual_now` and `fire_at` are in session time (see Session time below);
they equal wall-clock time unless the session clock was advanced.

`history` lists the most recent 100 state changes, oldest first. `cause`
is `created` (the session started in that state), `request` (a scenario
rule matched; `detail` is the request line), `webhook` (the first attempt of
a webhook fired its `goto`; `detail` is the event) or `admin` (a forced
transition). `at` is wall-clock time, `virtual_at` session time.

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

`GET /api/v1/deliveries?session=&target=&status=&limit=&cursor=` - the
delivery journal, newest first. `status` filters by `pending`, `retrying`,
`delivered`, `failed` or `canceled`. With `limit`, a response that has more
behind it carries `next_cursor`; pass it as `cursor` for the next page
(`400 cursor_expired` if that record has rotated out of the journal). Each record carries the full request (headers per attempt,
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
| `redeliver` | the same event again: the original body, so the same event id inside it, under a new `X-TwinStub-Delivery` and with a fresh signature. This is how a provider redelivers an event, so it tests whether your handler deduplicates on the event id rather than the delivery header. `body` is not allowed |
| `duplicate` | a byte-identical repeat: same body and the same `X-TwinStub-Delivery`, `-Event` and `-Session` as the original, with a fresh signature, the way provider retries look. A duplicate of a duplicate still carries the first id. `body` is not allowed |
| `bad_signature` | a well-formed signature that does not verify (`422 signing_not_configured` for an unsigned target) |
| `no_signature` | no signature header at all |

With `"wait": true` the call waits for the first attempt (up to `timeout`,
default `60s`) and returns `200` with the whole record under `delivery`,
including the receiver's status code and response body.

Journal records made by a replay carry `mode` (`replay`, `redeliver`, `duplicate`,
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

## Misses

`GET /api/v1/misses?limit=` - the most recent requests that matched no
endpoint and no scenario rule (up to 50), newest first. Each entry carries
the same diagnostics the 404 response did, plus `at`: method, path, the
session if the request reached one, and the closest candidates with the
reason each one rejected the request. Useful when your app swallowed the
404 and you want to know why after the fact.

```json
{"misses": [{
  "at": "2026-10-28T10:00:00Z", "error": "no_match",
  "method": "POST", "path": "/v1/refunds",
  "candidates": [{"endpoint": "get_rates", "method": "GET", "path": "/v1/rates",
                  "reason": "method POST does not match GET"}]
}], "count": 1}
```

## Reset

`POST /api/v1/reset` - a clean slate between test runs: every session is
deleted and every queued webhook canceled (session-bound or sent with
`/trigger`), session clocks are dropped, and by default the delivery
journal and the 404 log are cleared too. Keep either log with
`{"journal": false}` or `{"misses": false}`. To reset one session, use
`DELETE /api/v1/sessions/{id}`.

```json
{"webhooks_canceled": 3, "sessions_deleted": 2,
 "deliveries_cleared": 14, "misses_cleared": 1}
```

## Reload

`POST /api/v1/reload` - reload the config from disk, same code path as the
fsnotify watcher. An invalid config is rejected with 422 and the full
validation error list; the server keeps running on the previous version.
Active sessions are never interrupted by a reload.
