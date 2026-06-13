# payment.3ds

The challenge flow: the payment starts in `requires_action`, the customer
passes the 3DS challenge, then the payment completes and the webhook fires.

## 1. Create the payment

```sh
curl -s -X POST localhost:8080/v1/payments \
  -H 'X-TwinStub-Scenario: payment.3ds' \
  -H 'Content-Type: application/json' \
  -d '{"order_id": "ord_3001", "amount": 7900}'
```

```json
{"id": "pay_01...", "order_id": "ord_3001", "status": "requires_action",
 "next_action": {"type": "3ds_challenge", "url": "https://bank.example/3ds/01..."}}
```

## 2. Try a wrong code

```sh
curl -s -X POST localhost:8080/v1/payments/3ds/confirm \
  -H 'Content-Type: application/json' \
  -d '{"order_id": "ord_3001", "code": "000000"}'
# {"error": "invalid_3ds_code", "detail": "wrong code, the demo accepts 123456"}
```

The session stays in `awaiting_3ds`; you can retry.

## 3. Confirm with the demo code

```sh
curl -s -X POST localhost:8080/v1/payments/3ds/confirm \
  -H 'Content-Type: application/json' \
  -d '{"order_id": "ord_3001", "code": "123456"}'
# {"id": "pay_01...", "status": "processing"}
```

After ~2s the `payment.succeeded` webhook arrives with `"three_ds": "passed"`.
