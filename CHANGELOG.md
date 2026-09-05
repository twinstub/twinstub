# Changelog

All notable changes to this project are documented here. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/twinstub/twinstub/commits/main
