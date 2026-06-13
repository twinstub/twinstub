# TwinStub YAML DSL reference

Every config file must start with `version: 1`. Unknown fields are
rejected: a typo is an error, not a silent no-op. Run `twinstub validate`
to check a config; errors point at the file and field.

A project is a root config (`twinstub.yaml`) plus any number of included
files containing `endpoints:` and/or `scenarios:`.

## Root config

```yaml
version: 1

server:
  port: 8080              # public mock server
  admin:
    enabled: true         # default true
    port: 9090
    token: ${ADMIN_TOKEN} # optional bearer token; empty = no auth

defaults:
  latency: 0ms            # applied to every reply unless overridden
  scenario_header: X-TwinStub-Scenario

targets:                  # named webhook destinations (see Targets)
  merchant_callback:
    url: ${CALLBACK_URL}

limits:                   # optional runtime guards (defaults shown)
  max_sessions: 10000
  max_pending_webhooks_per_session: 100   # hard max 1000
  delivery_log_size: 1000
  webhook_workers: 8

include:                  # globs, relative to this file
  - endpoints/*.yaml
  - scenarios/*.yaml
```

Files matched by one glob merge in lexical order; globs merge in
declaration order.

### Environment variables

`${VAR}` in any string value is substituted at load time. A missing
variable is a validation error naming the variable. `${VAR:-default}`
supplies a fallback. Comments and keys are not substituted.

### Durations

Durations are strings: `150ms`, `30s`, `30m`, `72h`, `7d`. The `d` unit
cannot be combined with others (`1d2h` is invalid; use `26h`).

## Targets

```yaml
targets:
  merchant_callback:
    url: https://example.com/hooks     # http(s) only
    headers:                           # extra headers on every delivery
      X-Source: twinstub
    timeout: 10s                       # per-request timeout (default 10s)
    signing:                           # optional HMAC signing
      algorithm: hmac-sha256           # the only supported value
      secret: ${WEBHOOK_SECRET}
      header: X-TwinStub-Signature     # default
      format: "t={timestamp},v1={signature}"   # default, Stripe-style
    retry:
      attempts: 3                      # total attempts (default 3)
      backoff: 2s                      # exponential: 2s, 4s, 8s; +-20% jitter
```

Signing: with `{timestamp}` in the format the signed string is
`<unix_seconds>.<raw_body>`; with `format: "{signature}"` only the body is
signed. The signature is lowercase hex.

Delivery: POST with `Content-Type: application/json`, plus
`X-TwinStub-Delivery` (ULID), `X-TwinStub-Session`, and `X-TwinStub-Event`
when the webhook declares an `event`. Success is any 2xx. Redirects are
never followed. Non-2xx and transport errors are retried per the policy.

## Stateless endpoints

```yaml
version: 1
endpoints:
  - id: get_rates              # required, unique
    when:                      # the matcher, see below
      method: GET
      path: /v1/rates
      query:
        currency: "USD"
      headers:
        X-Api-Key: "re:^key_.+"
    reply:                     # see Replies
      status: 200
      headers: {Content-Type: application/json}
      latency: 150ms
      body: |
        {"currency": "USD", "rate": 1.0, "ts": "{{ now }}"}
```

Order matters: the first matching endpoint wins, within a file and across
includes (in merge order). There is no specificity scoring; this is a
deliberate choice for predictability. Put specific matchers above generic
fallbacks.

### The `when` matcher

| Field | Required | Semantics |
|---|---|---|
| `method` | yes | HTTP method, case-insensitive |
| `path` | yes | starts with `/`; `{param}` matches one segment, value available as `.Request.Params.param` |
| `query` | no | per-key match on the first value |
| `headers` | no | per-key match |
| `body` | no | list of JSON body conditions |

String values in `query` and `headers` are exact matches unless prefixed
with `re:`, which switches to RE2 regular expressions.

Quote paths with parameters inside flow mappings:
`when: {method: GET, path: "/v1/payments/{id}"}` (YAML would otherwise
parse `{id}` as a nested mapping).

Body conditions use a JSONPath subset (dot notation and array indices):

```yaml
body:
  - path: $.amount
    gt: 100000          # operators: equals, exists, matches, gt, lt
  - path: $.items[0].sku
    equals: "sku_1"
  - path: $.customer.email
    matches: ".*@example[.]com$"
  - path: $.metadata
    exists: true
```

Exactly one operator per condition. `equals` compares numbers numerically
and strings literally; `matches` is a regex on the string form; `gt`/`lt`
require numbers; `exists: false` asserts absence.

## Stateful scenarios

```yaml
version: 1
scenarios:
  - id: payment.chargeback     # selected via the scenario header
    description: "Payment succeeds, then a chargeback arrives"
    session:
      key: "body:$.order_id"   # what groups requests into one session
      ttl: 30m                 # sliding, default 30m, max 24h
    initial: new
    states:
      new:
        on:                    # request rules for this state
          - when: {method: POST, path: /v1/payments}
            reply: {status: 201, body: '...'}
            set:               # session variables (template strings)
              amount: "{{ .Request.Body.amount }}"
            goto: processing   # transition after replying
      processing:
        enter:                 # actions executed on entering the state
          - webhook:
              target: merchant_callback
              event: payment.succeeded   # optional, sets X-TwinStub-Event
              after: 3s                  # delay, max 7d, scaled by --time-scale
              body: '{"amount": {{ .Session.Vars.amount }}}'
            goto: succeeded    # fires after the FIRST delivery attempt
        on:
          - when: {method: GET, path: "/v1/payments/{id}"}
            reply: {status: 200, body: '{"status": "processing"}'}
      succeeded: {on: []}      # terminal: no enter transitions
```

Semantics:

- A state is a named node. `on` rules are evaluated in order; `reply` is
  required in each rule, `set` and `goto` are optional.
- `enter` actions run when the state is entered, including the initial
  state at session creation. v1.0 supports one action type: `webhook`.
- An action's `goto` fires after the first delivery attempt, regardless of
  the outcome; retries continue in the background. If the session has
  already moved to another state by then (e.g. via the admin API), the
  stale transition is dropped.
- The webhook body is rendered when the state is entered, so it sees the
  session variables as of that moment.
- A request that matches no rule of the current state falls through to the
  stateless endpoints, then to 404 diagnostics. The session stays put.
- Cycles in the state graph are allowed. `validate` checks that `goto` and
  `initial` reference existing states and warns about unreachable states.
- Limit: 200 states per scenario.

### Sessions

A request with the scenario header (`X-TwinStub-Scenario: <id>` by
default) creates a session if the extracted key has none yet. On later
requests the header is ignored; the session is found by its key.

Session key selectors:

| Selector | Source |
|---|---|
| `body:$.field` | JSONPath into the JSON body |
| `header:X-Name` | header value |
| `query:param` | query parameter |
| `path:{param}` | path parameter from any rule pattern of the scenario |

If the selector yields nothing, the reply is `422` with diagnostics. One
key can live in only one scenario at a time: starting a second scenario
with the same key returns `409` with an explanation. Sessions expire after
a sliding TTL; expiry cancels the session's pending webhooks.

`DELETE /api/v1/sessions/{id}` on the admin port resets a session early.

## Replies

```yaml
reply:
  status: 200          # default 200; 0 = close the connection (chaos)
  headers:             # values are templates too
    X-Request-ID: "{{ uuid }}"
  latency: 150ms       # sleep before responding (default from defaults.latency)
  body: |              # a Go text/template
    {"id": "{{ .Session.ID }}"}
```

If no `Content-Type` is set and the body looks like JSON, TwinStub sends
`application/json`. A template error at runtime returns a 500 with the
error text in the body: this is a development tool, errors are never
hidden. Bodies are capped at 5 MB.

## Templates

Go `text/template`. Context:

| Field | Content |
|---|---|
| `.Request.Method`, `.Request.Path` | strings |
| `.Request.Params.<name>` | path parameters |
| `.Request.Query.<name>` | first query value |
| `.Request.Headers.<name>` | header value (use `index` for names with dashes: `{{ index .Request.Headers "X-Api-Key" }}`) |
| `.Request.Body` | parsed JSON (map/slice) |
| `.Request.RawBody` | raw body string (set for non-JSON bodies too) |
| `.Session.ID` | engine-generated ULID |
| `.Session.Key`, `.Session.Scenario`, `.Session.State` | strings |
| `.Session.Vars.<name>` | variables written by `set` |

`.Session` is only set inside scenarios; referencing it in a stateless
endpoint is a render error (500).

Functions (the complete v1.0 set):

| Function | Result |
|---|---|
| `uuid` | random UUID v4 |
| `ulid` | ULID |
| `now` | current time, RFC3339 UTC |
| `nowUnix` | unix seconds |
| `addTime "72h"` | now + duration, RFC3339 UTC |
| `randInt 5 10` | integer in [5, 10] |
| `randString 12` | alphanumeric string |
| `upper s`, `lower s` | case conversion |
| `json v` | JSON-serialize a value |

With `--seed N`, `uuid`, `randInt` and `randString` are reproducible
across runs: useful for snapshot-style assertions in CI.

## Limits (validated and enforced at runtime)

| Parameter | Default | Hard max |
|---|---|---|
| Concurrent sessions | 10 000 | configurable |
| Pending webhooks per session | 100 | 1 000 |
| `after` interval | - | 7d |
| Reply/webhook body size | - | 5 MB |
| States per scenario | - | 200 |
| Session TTL | 30m | 24h |
