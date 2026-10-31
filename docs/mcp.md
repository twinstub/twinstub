# MCP server

`twinstub mcp` runs a [Model Context Protocol](https://modelcontextprotocol.io)
server on stdio. A coding agent (Claude Code, Codex, Cursor and other MCP
clients) uses it to drive TwinStub while it writes an integration: start a
session, deliver webhooks out of order, move a session's clock 30 days
forward, resend an event, and read what the app answered.

The server is a thin client of the [admin API](admin-api.md). It ships in
the same binary; nothing else to install.

## Setup

Any MCP client that starts a local command works. For Claude Code, a
project-level `.mcp.json`:

```json
{
  "mcpServers": {
    "twinstub": {"command": "twinstub", "args": ["mcp"]}
  }
}
```

Without the binary, run the image instead. Inside a container `localhost`
is the container itself, so point it at the engine on your machine:

```json
{
  "mcpServers": {
    "twinstub": {
      "command": "docker",
      "args": ["run", "-i", "--rm", "--add-host=host.docker.internal:host-gateway",
               "-e", "TWINSTUB_ADMIN_URL=http://host.docker.internal:9090",
               "ghcr.io/twinstub/twinstub", "mcp"]
    }
  }
}
```

`--add-host` makes `host.docker.internal` resolve on Linux too; Docker
Desktop on macOS and Windows has it already.

## Which engine it uses

1. `--admin-url` (or `TWINSTUB_ADMIN_URL`) is set: that engine and no
   other. If it does not answer, every tool says so and how to fix it;
   the server never falls back to an embedded engine.
2. Not set, and an engine answers at `http://localhost:9090`: that one
   (mode `external`). Run `twinstub serve` yourself when you want to watch
   its logs.
3. Not set and nothing answers: the server starts an engine in its own
   process (mode `embedded`) with `--config` (default `twinstub.yaml`). Its
   admin API listens on a random port on 127.0.0.1; its public port is the
   config's, or a free one if that is busy. Without a config file it starts
   empty on a free port, so a plugin installed for every project does not
   hold 8080 in projects that do not use TwinStub; the agent loads a config
   with `twinstub_load_config`, and the engine moves to that config's port.

`twinstub_status` always reports `mode`, `admin_url`, the engine
`version`, `config_path` and `public_port`, so the agent knows where to
point the app.

| Flag | Environment | Default | |
|---|---|---|---|
| `--admin-url` | `TWINSTUB_ADMIN_URL` | | use this engine only |
| `--token` | `TWINSTUB_TOKEN` | | admin API bearer token |
| `--config`, `-c` | `TWINSTUB_CONFIG` | `twinstub.yaml` | config of the embedded engine |
| `--timeout` | `TWINSTUB_TIMEOUT` | `60s` | how long one call may wait for deliveries |

stdout carries only the protocol. All logs, including the embedded
engine's, go to stderr.

## Tools

| Tool | What it does |
|---|---|
| `twinstub_status` | engine mode, URL, version, config path, `public_port` |
| `twinstub_list_scenarios` | states, rules, webhooks with delays and targets, session key selector |
| `twinstub_validate_config` | check a file (`path`) or YAML (`content`); errors with file, line and field |
| `twinstub_load_config` | load a config; the embedded engine restarts on the same public port and sessions end |
| `twinstub_start_session` | start a session for a key, optionally in a given state |
| `twinstub_get_session` | state, transition history, `virtual_now`, queued webhooks |
| `twinstub_trigger` | move a session to a state; its webhooks are scheduled |
| `twinstub_advance_time` | move a session's clock (`30d`) and deliver everything due, in order |
| `twinstub_deliver` | send queued webhooks now, in a given order |
| `twinstub_list_deliveries` | the delivery journal: event, mode, attempts, status code, signature valid or not |
| `twinstub_replay_delivery` | resend a delivery: `as_is`, `redeliver`, `duplicate`, `bad_signature`, `no_signature` |
| `twinstub_explain_miss` | recent 404s with the closest rules and why each rejected the request |
| `twinstub_reset` | delete one session, or reset everything between test runs |

Every tool declares its hints: read-only tools are marked read-only,
`twinstub_load_config` and `twinstub_reset` destructive, and none of them
reaches outside the engine (`openWorldHint: false`).

`twinstub_replay_delivery` modes are described in the admin API under
[Deliveries](admin-api.md#deliveries). `redeliver` is the usual
idempotency test: the same event under a new delivery id.

## Answers and errors

Answers are compact JSON. Lists return 20 items by default (at most 100)
with a `next_cursor`. Bodies are cut to 500 bytes in lists and 16 KB for a
single delivery; a cut sets `truncated: true` and the original size.

An error is one message that says what went wrong and what to call next,
for example:

```
Engine not reachable at http://localhost:9091 (set via --admin-url or TWINSTUB_ADMIN_URL). Start it with `twinstub serve` or remove the setting to use the embedded engine.
```

`twinstub_status` also checks that an external engine is recent enough
for these tools (1.1.0 or newer) and says so if not.

## Limits

- `twinstub_advance_time` moves TwinStub's clock, not the app's. Logic on
  the app side that depends on time needs an injectable clock to be tested
  the same way.
- The server runs no shell commands and reads no files except the configs
  passed to `twinstub_validate_config` and `twinstub_load_config` (and the
  files those include).
- An external engine can only reload the config it was started with; to
  switch files, restart it with `twinstub serve -c <file>`.
