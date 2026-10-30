# agent-demo

A small subscription service in Go. A customer subscribes to a plan, pays
through Acme Pay, and gets access and credits when Acme Pay reports the
payment as completed. Acme Pay's API and webhooks are simulated by
TwinStub with [twinstub.yaml](twinstub.yaml).

| | |
|---|---|
| `POST /subscribe` `{"user": "u1", "plan": "basic"}` | starts a payment at Acme Pay |
| `GET /accounts/{user}` | status, credits and whether the user has access |
| `POST /webhooks/acme` | Acme Pay events, signed in the `Acme-Signature` header |

The unit tests pass:

```sh
go test ./...
```

## Run it

```sh
go run .                 # the service on :3000, calling Acme Pay at localhost:8080
twinstub serve           # Acme Pay, in another terminal (or let twinstub mcp start it)
```

```sh
curl -s -X POST localhost:3000/subscribe -d '{"user": "u1", "plan": "basic"}'
curl -s localhost:3000/accounts/u1
```

Acme Pay reports the payment as pending after 30 seconds and as completed
after 2 minutes; a chargeback follows 30 days later.

## Hand it to an agent

Open Claude Code in this directory with the
[TwinStub plugin](https://github.com/twinstub/claude-plugin) installed and
ask:

> Check the Acme Pay webhook handling of this service for edge cases.

The service has webhook bugs that its tests do not catch. Every webhook
it gets is answered with 200, so a check of status codes alone finds
nothing; the agent has to look at what the service did.
