# TwinStub

[![CI](https://github.com/twinstub/twinstub/actions/workflows/ci.yaml/badge.svg)](https://github.com/twinstub/twinstub/actions/workflows/ci.yaml)
[![Release](https://img.shields.io/github/v/release/twinstub/twinstub)](https://github.com/twinstub/twinstub/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Deterministic API simulation for teams whose customers integrate with
their API. Describe stateful scenarios in YAML; TwinStub plays them back
as a real HTTP server with sessions, state and chains of signed webhooks:
"payment succeeds, 30 days later a chargeback arrives" compressed into
seconds.

## Why not just a mock server?

If you provide an API (payments, logistics, CRM), your customers build
against it. Their hardest bugs are not in the happy path: they are in the
chargeback that arrives a month later, the KYC rejection, the webhook
signature their code never validates, the 3DS challenge flow. Reproducing
those against a sandbox is slow and flaky, and classic mock servers answer
one request at a time without memory.

TwinStub models an integration as a state machine. A session remembers
where each client is in the flow: the same `GET /v1/payments/{id}` returns
`processing`, then `succeeded`, then `chargeback`, depending on what
happened before. Timed webhooks fire between states, HMAC-signed like the
real thing, with retries, a delivery journal and replay.

It is a single binary you run locally or in CI. No cloud, no registration,
MIT.

## What you actually test

TwinStub is the provider, faked. The code under test is yours: the client
that builds the request, parses the response, receives the callback,
verifies its signature and moves your own order or ledger forward. That
code is full of branches that only run on rare events, and those branches
are where integrations break:

- **Inbound webhooks**: does your handler verify the signature and reject a
  forged one? does it answer 2xx so the sender stops retrying?
- **Unhappy endings**: on an expired or failed payment, do you release the
  goods you reserved and avoid crediting money that never arrived?
- **Idempotency**: the same event delivered twice (a retry) must settle
  once, not twice.
- **Out-of-order delivery**: `completed` can arrive before you finished
  handling `pending`.
- **Transport faults**: a 3s stall or a dropped connection should retry or
  fail cleanly, not hang.

None of these need the provider's real servers, keys or money, and most
cannot be triggered on demand in a provider sandbox at all. Think of it as
a crash-test dummy: the provider is the stand-in, the safety being measured
is your own code's.

One honest limit: a twin is only as accurate as the scenario you write. If
the YAML mismodels the real response, the twin repeats your mistake instead
of catching it. So confirm the response shape once against the provider's
own sandbox, then use TwinStub to drive the hundreds of edge cases the
sandbox cannot.

## 5 minutes to the first webhook

```sh
go install github.com/twinstub/twinstub/cmd/twinstub@latest
# or: docker run -v $PWD:/work -p 8080:8080 -p 9090:9090 ghcr.io/twinstub/twinstub serve
# or grab a binary from Releases

twinstub init demo && cd demo
twinstub serve
```

In another terminal, start any webhook receiver on :9999 (a one-liner is
in the generated README), then:

```sh
curl -s -X POST localhost:8080/v1/orders/ord_42/payment \
  -H 'X-TwinStub-Scenario: payment.chargeback' \
  -H 'Content-Type: application/json' \
  -d '{"amount": 1999}'
```

You get a synchronous `201` with a payment id. Three seconds later your
receiver gets a signed `payment.succeeded` webhook; ten seconds after that,
`payment.chargeback`. Poll `GET /v1/orders/ord_42/payment` in between and
watch its status change. That whole flow is [one YAML file](internal/scaffold/templates/scenarios/payment_chargeback.yaml).

## What is in the box

- **Stateless endpoints**: method/path/query/header/JSON-body matching,
  regex via `re:`, templated replies, latency injection - the WireMock
  level, declared in YAML.
- **Stateful scenarios**: state machines with sessions (keyed by body
  field, header, query or path param), session variables, sliding TTL.
- **Webhook chains**: delayed, HMAC-signed (Stripe-compatible format),
  exponential retries with jitter, a delivery journal, replay and edit.
- **Time compression**: `--time-scale 0.01` turns 30 days into ~7 hours;
  `--seed` makes random template functions reproducible in CI.
- **Admin API** on a separate port: sessions, scenarios, delivery journal,
  replay, manual trigger, reload.
- **Hot reload** that never breaks active sessions: they finish on the
  config version they started with.
- **Diagnostic 404s**: every miss lists the closest matchers and the exact
  reason each one rejected the request.
- **Chaos tools**: `status: 0` drops the TCP connection, latency of 30s+,
  arbitrary 5xx.

## Honest comparison

| | TwinStub | WireMock | Mockoon | Beeceptor |
|---|---|---|---|---|
| Stateful scenario chains | yes, first-class | scenarios (flat states) | limited (rules) | no |
| Timed webhook chains | yes, with time scale | per-request callbacks | partial (callbacks) | basic callbacks |
| HMAC-signed webhooks | yes | manual setup | no | no |
| Delivery journal + replay | yes | no | no | paid |
| Single static binary | yes | JVM | Electron/CLI | SaaS |
| Record and proxy real traffic | no | yes, mature | yes | yes |
| GUI | no (by design) | third-party | yes, great | yes |
| Protocol coverage | HTTP/JSON | HTTP incl. multipart, XML | HTTP, WS, gRPC proxying | HTTP |

If you need traffic recording, a GUI, or XML/SOAP matching, WireMock and
Mockoon are better tools today. TwinStub's lane is deterministic,
versionable simulation of event-driven API products.

## Documentation

- [DSL reference](docs/dsl.md) - every field of the YAML format
- [Admin API](docs/admin-api.md)
- [Fintech scenario catalog](examples/fintech/) - 8 ready-made flows:
  success, decline, 3DS, chargeback, refunds, KYC, broken signatures,
  chaos
- [Webhook signature verification](examples/verify-signature/) - Go and
  Node snippets

## CLI

```
twinstub serve [-c twinstub.yaml] [--port N] [--admin-port N]
               [--time-scale F] [--seed N] [--log-format json|pretty]
               [--log-level L] [--allow-private-targets]
twinstub validate [-c twinstub.yaml]   # exit 0/1, errors with file and field
twinstub init [dir]                    # scaffold a working example
twinstub version
```

Every flag has a `TWINSTUB_*` environment variable twin (the flag wins).
`twinstub validate` is built for CI: one command, precise errors.

## Operational notes

- State is in-memory by design; a restart clears sessions and the
  delivery journal. Persistence is out of scope for v1.0.
- Run TLS termination and auth for the public port in your reverse proxy.
- The admin port should not be exposed publicly; set
  `server.admin.token` when it leaves localhost.
- `--allow-private-targets=false` blocks webhook delivery to loopback,
  RFC1918, link-local and cloud metadata addresses, and webhook redirects
  are never followed.
- Matching is declaration-order, first match wins; there is no specificity
  scoring on purpose. Put specific rules first.

## Development

```sh
make test     # go test -race ./...
make lint     # golangci-lint
make bench    # throughput benchmark (NFR: 1000+ rps stateless)
make build    # bin/twinstub
```

Contributions welcome. Note that the YAML DSL and the admin API are
stable contracts: changes there need a discussion issue first.

## License

MIT
