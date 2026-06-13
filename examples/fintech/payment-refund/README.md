# payment.refund

Partial and full refunds. The demo "full price" is 1000: a refund below
that is partial, anything else is full. Refunding an already refunded
payment returns 409.

## 1. Pay

```sh
curl -s -X POST localhost:8080/v1/payments \
  -H 'X-TwinStub-Scenario: payment.refund' \
  -H 'Content-Type: application/json' \
  -d '{"order_id": "ord_5001", "amount": 1000}'
# {"id": "pay_01...", "order_id": "ord_5001", "amount": 1000, "status": "succeeded"}
```

## 2. Partial refund

```sh
curl -s -X POST localhost:8080/v1/refunds \
  -H 'Content-Type: application/json' \
  -d '{"order_id": "ord_5001", "amount": 300}'
# {"refund_id": "re_...", "amount": 300, "status": "partial_refund"}
```

Webhook ~2s later: `payment.refund.partial`.

## 3. Refund the rest

```sh
curl -s -X POST localhost:8080/v1/refunds \
  -H 'Content-Type: application/json' \
  -d '{"order_id": "ord_5001", "amount": 700}'
# {"refund_id": "re_...", "status": "refunded"}
```

Webhook: `payment.refund.full`. A further refund attempt:

```sh
curl -s -X POST localhost:8080/v1/refunds \
  -H 'Content-Type: application/json' -d '{"order_id": "ord_5001", "amount": 1}'
# {"error": "already_refunded"}   (HTTP 409)
```
