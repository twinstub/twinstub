# kyc.flow

KYC verification with two outcomes, modeled as two scenarios. Your client
code is identical for both; only the scenario header picks the branch, so
one integration test suite covers both paths.

Sessions are keyed by the `X-Applicant-ID` header. Note: the enter actions
of the initial state run at session creation, so the decision webhook is
scheduled the moment the application is submitted (5s later).

## Approved branch

```sh
curl -s -X POST localhost:8080/v1/kyc/applications \
  -H 'X-TwinStub-Scenario: kyc.flow.approved' \
  -H 'X-Applicant-ID: app_7001' \
  -d '{}'
# {"application_id": "kyc_01...", "applicant_id": "app_7001", "status": "pending"}

curl -s localhost:8080/v1/kyc/applications/kyc_x -H 'X-Applicant-ID: app_7001'
# {"status": "pending"}   then after ~5s: {"status": "approved"}
```

Webhook: `{"event": "kyc.approved", ...}`

## Rejected branch

```sh
curl -s -X POST localhost:8080/v1/kyc/applications \
  -H 'X-TwinStub-Scenario: kyc.flow.rejected' \
  -H 'X-Applicant-ID: app_7002' \
  -d '{}'
```

Webhook: `{"event": "kyc.rejected", "reason": "document_unreadable"}`

Note: one applicant id can only run one scenario at a time. Reusing
`app_7001` with the rejected branch while the approved session is alive
returns 409 with an explanation.
