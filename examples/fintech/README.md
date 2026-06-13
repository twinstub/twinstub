# Fintech scenario catalog

Eight ready-to-run payment provider simulations. One config loads them all:

```sh
cd examples/fintech
twinstub serve
```

Webhooks go to `CALLBACK_URL` (default `http://localhost:9999/webhook`).
A one-line receiver for experiments:

```sh
python3 -c '
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        print(self.headers.get("X-TwinStub-Event"), self.rfile.read(n).decode())
        self.send_response(200); self.end_headers()
http.server.HTTPServer(("", 9999), H).serve_forever()
'
```

Tip: run `twinstub serve --time-scale 0.1` to compress every webhook delay
ten times.

| Scenario | Header value | What it shows |
|---|---|---|
| [payment-success](payment-success/) | `payment.success` | Payment plus confirmation webhook |
| [payment-declined](payment-declined/) | `payment.declined` | Synchronous decline (insufficient_funds) |
| [payment-3ds](payment-3ds/) | `payment.3ds` | Challenge flow: pending, 3DS, confirm, success |
| [payment-chargeback](payment-chargeback/) | `payment.chargeback` | Success, then a chargeback 30 days later |
| [payment-refund](payment-refund/) | `payment.refund` | Partial and full refunds |
| [kyc-flow](kyc-flow/) | `kyc.flow.approved` / `kyc.flow.rejected` | KYC pending to approved or rejected |
| [webhook-bad-signature](webhook-bad-signature/) | `webhook.bad_signature` | Deliberately invalid HMAC for testing validation |
| [chaos-timeouts](chaos-timeouts/) | `chaos.timeouts` | 30s+ latency, 502/503, dropped connections |

Each directory has a README with curl commands and expected output.
