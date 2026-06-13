# chaos.timeouts

Failure modes for testing client resilience: long latency, 5xx responses,
dropped connections, and a flaky upstream that recovers.

## Stateless chaos endpoints

```sh
curl -s --max-time 5 localhost:8080/chaos/slow
# curl: (28) Operation timed out   (the reply takes 35s)

curl -s -i localhost:8080/chaos/502 | head -1
# HTTP/1.1 502 Bad Gateway

curl -s -i localhost:8080/chaos/503 | head -3
# HTTP/1.1 503 Service Unavailable
# Retry-After: 30

curl -s localhost:8080/chaos/drop
# curl: (52) Empty reply from server   (status: 0 closes the connection)
```

## Flaky upstream scenario

The same request fails twice in different ways, then succeeds: exactly what
a retry implementation needs to be tested against. Requests are grouped by
the `X-Request-Group` header.

```sh
H='-H "X-Request-Group: grp_1"'
curl -s -i -H 'X-TwinStub-Scenario: chaos.timeouts' -H 'X-Request-Group: grp_1' \
  localhost:8080/v1/balance | head -1
# HTTP/1.1 503 Service Unavailable   (after a 2s delay)

curl -s -H 'X-Request-Group: grp_1' localhost:8080/v1/balance
# curl: (52) Empty reply from server

curl -s -H 'X-Request-Group: grp_1' localhost:8080/v1/balance
# {"balance": 104250, "currency": "EUR"}
```
