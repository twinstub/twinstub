# webhook.bad_signature

A negative test for your webhook signature validation. The target
`bad_signature_callback` signs with the secret `whsec_intentionally_wrong`,
while your receiver validates against the real one
(`WEBHOOK_SECRET`, default `whsec_fintech_demo`).

If your endpoint accepts this webhook, your validation is broken.

```sh
curl -s -X POST localhost:8080/v1/payments \
  -H 'X-TwinStub-Scenario: webhook.bad_signature' \
  -H 'Content-Type: application/json' \
  -d '{"order_id": "ord_6001", "amount": 100}'
```

After ~1s the receiver gets a webhook whose `X-TwinStub-Signature` header
does NOT verify against `whsec_fintech_demo`. The expected receiver
behavior is to reject it (400/401) and never process the event.

Reference validation snippets (Go and Node) live in
[examples/verify-signature](../../verify-signature/).

Tip: respond with a non-2xx status and watch TwinStub retry with backoff:

```sh
curl -s 'localhost:9090/api/v1/deliveries?target=bad_signature_callback'
```
