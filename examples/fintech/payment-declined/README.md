# payment.declined

A synchronous decline with `insufficient_funds`, followed by a
`payment.declined` webhook. The terminal state keeps answering status polls
until the session TTL expires.

```sh
curl -s -i -X POST localhost:8080/v1/payments \
  -H 'X-TwinStub-Scenario: payment.declined' \
  -H 'Content-Type: application/json' \
  -d '{"order_id": "ord_2001", "amount": 99999}'
```

Expected: HTTP 402 with

```json
{"id": "pay_01...", "order_id": "ord_2001", "status": "declined", "decline_code": "insufficient_funds"}
```

The webhook (~1s later):

```json
{"event": "payment.declined", "payment_id": "pay_01...", "order_id": "ord_2001", "decline_code": "insufficient_funds"}
```
