# Changelog

All notable changes to this project are documented here. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Session clocks**: every session runs on its own virtual clock.
  `POST /api/v1/sessions/{id}/advance-time` moves it forward and replays
  everything due in the window in order (webhooks, the transitions they
  trigger, the webhooks those schedule, retries), so "a chargeback 30 days
  later" plays out in one call. Other sessions stay on wall time; signature
  timestamps and session TTL stay on wall time.
- Template functions `now`, `nowUnix` and `addTime` render session time
  inside scenarios.
- `{{ .Webhook.At }}` and `{{ .Webhook.AtUnix }}` in webhook bodies: the
  session time the delivery is due.
- Admin API: `virtual_now` and `clock_offset` in session details; delivery
  attempts in an advanced session carry `virtual_at`.
- `POST /api/v1/sessions/{id}/deliver` sends a session's queued webhooks
  now, in a given order, to stage races such as "completed arrives before
  pending". Items are event names or delivery ids; the session clock does
  not move.

### Changed

- `fire_at` of pending webhooks in session details is reported in session
  time. It is unchanged for sessions that were never advanced.
- The webhook `after` limit is raised from 7d to 365d. Session clocks make
  long delays practical (chargebacks, refund windows, renewals); existing
  configs stay valid.

## [1.0.0] - 2026-09-05

First open-source release of TwinStub OSS core.

### Added

- **Stateless endpoints**: method / path / query / header / JSON-body
  matching, `re:` regex, templated replies, latency injection.
- **Stateful scenarios**: state machines with sessions keyed by body field,
  header, query or path param, session variables, and a sliding TTL.
- **Webhook chains**: delayed, HMAC-signed (Stripe-compatible format),
  exponential retries with jitter, a delivery journal, replay and edit.
- **Time compression** via `--time-scale`, and reproducible template
  randomness via `--seed`.
- **Admin API** on a separate port: sessions, scenarios, delivery journal,
  replay, manual trigger, reload.
- **Hot reload** that keeps active sessions on the config version they
  started with.
- **Diagnostic 404s** listing the closest matchers and why each was
  rejected.
- **SSRF protection** for webhook delivery (`--allow-private-targets`), with
  redirects never followed.
- CLI: `serve`, `validate`, `init`, `version`, each flag mirrored by a
  `TWINSTUB_*` environment variable.
- Example catalog: quickstart project plus eight fintech scenarios, and
  Go / Node webhook signature-verification snippets.

[Unreleased]: https://github.com/twinstub/twinstub/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/twinstub/twinstub/releases/tag/v1.0.0
