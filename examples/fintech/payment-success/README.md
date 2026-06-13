# payment.success

The happy path: a payment is accepted synchronously, the confirmation
webhook arrives 2 seconds later.

```sh
curl -s -X POST localhost:8080/v1/payments \
  -H 'X-TwinStub-Scenario: payment.success' \
  -H 'Content-Type: application/json' \
  -d '{"order_id": "ord_1001", "amount": 2500}'
```

Expected reply:

```json
{"id": "pay_01...", "order_id": "ord_1001", "amount": 2500, "status": "processing"}
```

After ~2s the receiver gets a signed webhook:

```json
{"event": "payment.succeeded", "payment_id": "pay_01...", "order_id": "ord_1001", "amount": 2500}
```

Poll the state (the `order_id` in the body keeps you in the session):

```sh
curl -s localhost:8080/v1/payments/pay_x -X GET \
  -H 'Content-Type: application/json' -d '{"order_id": "ord_1001"}'
# before the webhook: {"status": "processing"}
# after:              {"status": "succeeded"}
```
