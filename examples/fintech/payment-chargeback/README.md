# payment.chargeback

The flagship demo: "payment succeeded, 30 days later a chargeback arrives",
compressed into 13 seconds. In real configs you would keep the production
interval (`after: 30d`) and compress it with `--time-scale` in tests.

```sh
curl -s -X POST localhost:8080/v1/payments \
  -H 'X-TwinStub-Scenario: payment.chargeback' \
  -H 'Content-Type: application/json' \
  -d '{"order_id": "ord_4001", "amount": 1999}'
# {"id": "pay_01...", "order_id": "ord_4001", "status": "processing"}
```

Timeline on the webhook receiver:

- t+3s: `payment.succeeded` with the amount
- t+13s: `payment.chargeback` with `"reason": "fraudulent"`

State machine memory between the webhooks:

```sh
curl -s localhost:8080/v1/payments/pay_x -X GET \
  -H 'Content-Type: application/json' -d '{"order_id": "ord_4001"}'
# t+0..3s:   {"status": "processing"}
# t+3..13s:  {"status": "succeeded"}
# t+13s..:   {"status": "chargeback"}
```

Inspect and replay deliveries:

```sh
curl -s localhost:9090/api/v1/deliveries | head -50
curl -s -X POST localhost:9090/api/v1/deliveries/<id>/replay
```
