# Verifying TwinStub webhook signatures

TwinStub signs every webhook with HMAC-SHA256. With the default
Stripe-style format `t={timestamp},v1={signature}` the signed string is:

```
<unix_timestamp>.<raw_request_body>
```

With `format: "{signature}"` only the raw body is signed.

The signature is lowercase hex in the header configured by
`signing.header` (default `X-TwinStub-Signature`).

Rules for a correct implementation:

1. Read the RAW request body bytes. Do not re-serialize parsed JSON.
2. Parse `t` and `v1` from the header.
3. Compute `HMAC_SHA256(secret, t + "." + body)` and hex-encode.
4. Compare with a constant-time comparison.
5. Reject if `t` is too old (replay protection); 5 minutes is a sane window.

Run the snippets against a live TwinStub:

```sh
# Go receiver on :9999
cd go && go run .

# or Node receiver on :9999
cd node && node verify.js

# then, from examples/fintech:
twinstub serve
curl -s -X POST localhost:8080/v1/payments \
  -H 'X-TwinStub-Scenario: payment.success' \
  -H 'Content-Type: application/json' \
  -d '{"order_id": "ord_sig", "amount": 100}'
```

The receiver prints `signature OK` for `payment.success` and
`signature INVALID` for the `webhook.bad_signature` scenario.
