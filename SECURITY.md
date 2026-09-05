# Security Policy

## Supported versions

TwinStub is pre-1.0 in the open. Security fixes land on `main` and in the
latest tagged release. Older tags are not patched; upgrade to the latest
release to receive fixes.

## Reporting a vulnerability

Please report security issues **privately** - do not open a public issue,
pull request, or discussion for anything you believe is exploitable.

Use GitHub's private vulnerability reporting:

1. Go to the [Security tab](https://github.com/twinstub/twinstub/security).
2. Click **Report a vulnerability**
   ([direct link](https://github.com/twinstub/twinstub/security/advisories/new)).
3. Describe the issue and how to reproduce it.

This routes the report straight to the maintainers through GitHub, so no
email address is involved.

A useful report includes:

- the version or commit affected,
- a minimal config or steps to reproduce,
- the impact you observed, and
- any suggested fix, if you have one.

## What to expect

- We aim to acknowledge a report within a few days.
- We will confirm the issue, work on a fix, and keep you updated on
  progress.
- Once a fix is released we will publish a security advisory and credit you
  (unless you prefer to stay anonymous).

## Scope notes

TwinStub is a local/CI simulation tool, not a hardened public service. Some
behaviour is intentional and out of scope for a vulnerability report:

- **The admin API has no auth by default.** It is meant for localhost. Set
  `server.admin.token` before exposing that port, and terminate TLS and
  authentication in front of the public port in your own reverse proxy.
- **Template errors return 500 with the error text in the body.** This is a
  development tool by design; do not run it as an untrusted public endpoint.
- **In-memory state only.** A restart clears sessions and the delivery
  journal.

Things that *are* in scope and worth reporting include: bypassing the
outbound SSRF protections for webhook delivery (loopback / RFC1918 /
link-local / cloud metadata, or redirect-based bypasses), webhook signature
forgery or verification flaws, and any way to read or write beyond the
configured scope.
