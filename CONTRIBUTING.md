# Contributing to TwinStub

Thanks for your interest in improving TwinStub. Bug reports, docs fixes,
new example scenarios and code contributions are all welcome.

## Ways to contribute

- **Report a bug** or request a feature via
  [Issues](https://github.com/twinstub/twinstub/issues). Please search
  first to avoid duplicates.
- **Add an example scenario.** The `examples/` tree is a catalog; a
  well-documented real-world flow (a payment edge case, a webhook quirk) is
  a great first contribution.
- **Improve the docs** in `README.md` and `docs/`.
- **Fix a bug or add a feature** - see the workflow below.

## Before you change the DSL or the admin API

The YAML DSL (`docs/dsl.md`) and the admin API (`docs/admin-api.md`) are
**stable contracts**. Changes there - new fields, renamed keys, altered
semantics - need a discussion issue **first**, so we agree on the shape
before code is written. This keeps existing user configs from breaking.

Everything else (internal refactors, bug fixes, new examples, docs) can go
straight to a pull request.

## Development setup

You need Go 1.26+.

```sh
make test     # go test -race ./...
make lint     # golangci-lint
make bench    # throughput benchmark
make build    # bin/twinstub
```

## Pull request workflow

1. Fork and branch from `main`.
2. Keep the change focused; one logical change per PR.
3. Add or update tests - new behaviour needs a test, bug fixes need a
   regression test.
4. Run `make test` and `make lint` locally; CI runs both on Linux, macOS
   and Windows and must be green.
5. Update `docs/` and `CHANGELOG.md` if your change is user-visible.
6. Write a clear PR description: what changed and why.

Commit messages follow a lightweight conventional style, matching the
existing history: `feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`.

## Code of conduct

This project follows the [Code of Conduct](CODE_OF_CONDUCT.md). By
participating you agree to uphold it.

## Reporting security issues

Do not open a public issue for a vulnerability. See
[SECURITY.md](SECURITY.md) for the private reporting channel.

## License

By contributing, you agree that your contributions are licensed under the
MIT License, the same as the project.
