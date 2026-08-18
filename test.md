# Observability Gateway — Lab Test Plan

## Prerequisites

- A running k8s / k3s cluster or Docker Compose with:
  - **Loki** at `http://loki:3100`
  - **Mimir** at `http://mimir:9009`
  - **Tempo** at `http://tempo:4318`
- Gateway binary built: `go build -o gateway ./cmd/gateway`
- Tools available: `curl`, `promtool`, `logcli`, `otelcol` (or grpcurl), `tcpdump`/wireshark

---

## 1. Gateway Startup & Config Validation

### 1.1 Healthy start

```bash
cat > config.yaml <<EOF
version: 1
gateway:
  port: 8080
  token: "lab-token"
  timeouts:
    query: 30s
    push: 60s
instances:
  - name: loki-prod
    backend: loki
    url: http://loki:3100
  - name: mimir-prod
    backend: mimir
    url: http://mimir:9009
  - name: tempo-prod
    backend: tempo
    url: http://tempo:4318
EOF
./gateway --config config.yaml
```

✅ Expect: gateway starts, logs show listening on `:8080`.

### 1.2 Missing token rejects startup

Remove `token:` from the `gateway:` section.
✅ Expect: non-zero exit with a clear config error message.

### 1.3 Invalid backend

Set `backend: influxdb` on one instance.
✅ Expect: startup error mentioning `unknown backend`.

### 1.4 Duplicate instance name

Add two instances with `name: loki-prod`.
✅ Expect: startup error mentioning `duplicate instance name`.

### 1.5 Both `url` and `push_urls` set

Set both fields on one instance.
✅ Expect: startup error mentioning `both url and push_urls set`.

### 1.6 `auth_passthrough: true` with `basic_auth` set

Set both fields on the same instance.
✅ Expect: startup error mentioning `auth_passthrough` conflict.

### 1.7 Tempo with `push_urls`

Add `push_urls:` to a tempo instance.
✅ Expect: startup error mentioning tempo cannot have push_urls.

### 1.8 Tempo with `labels` config

Add `labels:` to a tempo instance.
✅ Expect: startup error mentioning tempo cannot have labels config.

---

## 2. Authentication

### 2.1 No token → 401

```bash
curl -i -X POST http://localhost:8080/api/loki/push \
  -H "Content-Type: application/json" \
  -d '{"streams":[]}'
```

✅ Expect: `401 Unauthorized`, JSON body `{"error":"unauthorized"}`.

### 2.2 Wrong token → 401

```bash
curl -i -X POST http://localhost:8080/api/loki/push \
  -H "Authorization: Bearer wrong-token" \
  -H "Content-Type: application/json" \
  -d '{"streams":[]}'
```

✅ Expect: `401`.

### 2.3 Correct token accepted

```bash
TS=$(date +%s)000000000
curl -i -X POST http://localhost:8080/api/loki/push \
  -H "Authorization: Bearer lab-token" \
  -H "Content-Type: application/json" \
  -d "{\"streams\":[{\"stream\":{\"app\":\"test\"},\"values\":[[\"$TS\",\"hello lab\"]]}]}"
```

✅ Expect: `204 No Content`.

### 2.4 `/healthz` needs no token

```bash
curl -i http://localhost:8080/healthz
```

✅ Expect: `200 OK`.

### 2.5 `/metrics` needs no token

```bash
curl -s http://localhost:8080/metrics | grep gateway_
```

✅ Expect: Prometheus metrics output including `gateway_fanout_requests_total`.

---

## 3. Loki Push & Query

### 3.1 Push and query back

```bash
TS=$(date +%s)000000000
curl -s -X POST http://localhost:8080/api/loki/push \
  -H "Authorization: Bearer lab-token" \
  -H "Content-Type: application/json" \
  -d "{\"streams\":[{\"stream\":{\"app\":\"gateway-lab\"},\"values\":[[\"$TS\",\"hello from lab\"]]}]}"

# Query back
curl -s "http://localhost:8080/api/loki/query_range?query={app=%22gateway-lab%22}&limit=5&start=$(date -v-5M +%s 2>/dev/null || date -d '-5 minutes' +%s)000000000&end=$(date +%s)000000000" \
  -H "Authorization: Bearer lab-token"
```

✅ Expect: `204` on push; query returns the log line `"hello from lab"`.

### 3.2 No matching instance → 404

```bash
curl -i -X POST http://localhost:8080/api/loki/push \
  -H "Authorization: Bearer lab-token" \
  -H "Content-Type: application/json" \
  -d '{"streams":[]}'
```

✅ Expect: `404`, JSON `{"error":"no matching instance"}`.

### 3.3 Labels endpoint

```bash
curl -s "http://localhost:8080/api/loki/labels" \
  -H "Authorization: Bearer lab-token"
```

✅ Expect: `200` with JSON label names list.

### 3.5 Series endpoint

```bash
curl -s "http://localhost:8080/api/loki/series?match[]={app=%22gateway-lab%22}" \
  -H "Authorization: Bearer lab-token"
```

✅ Expect: `200` with matching series.

---

## 4. Loki Label Rewrite

Update `config.yaml` to add label rewrite to `loki-prod`:

```yaml
instances:
  - name: loki-prod
    backend: loki
    url: http://loki:3100
    labels:
      filter:
        mode: denylist
        names: [secret_key]
      inject:
        env: production
        gateway: "true"
```

Restart the gateway.

### 4.1 Injected labels appear, denylisted label absent

```bash
TS=$(date +%s)000000000
curl -s -X POST http://localhost:8080/api/loki/push \
  -H "Authorization: Bearer lab-token" \
  -H "Content-Type: application/json" \
  -d "{\"streams\":[{\"stream\":{\"app\":\"rewrite-test\",\"secret_key\":\"abc123\"},\"values\":[[\"$TS\",\"label rewrite test\"]]}]}"
```

Query Loki **directly** (bypass gateway to see raw stored labels):

```bash
curl -s "http://loki:3100/loki/api/v1/query?query={app=%22rewrite-test%22}"
```

✅ Expect: Labels include `env="production"` and `gateway="true"`. Label `secret_key` is **absent**.

### 4.2 Allowlist mode

Change filter to `mode: allowlist`, `names: [app]`. Restart.
Push with labels `{app: "x", noise: "y"}`. Query Loki directly.
✅ Expect: Only `app`, `env`, `gateway` labels remain; `noise` is absent.

---

## 5. Mimir Push & Query

### 5.1 Basic push via Prometheus remote_write

Configure a Prometheus instance to remote_write to:
```
http://localhost:8080/api/mimir/push
```
With header `Authorization: Bearer lab-token`. Let it scrape for 30 seconds, then:

```bash
curl -s "http://localhost:8080/api/mimir/query?query=up" \
  -H "Authorization: Bearer lab-token"
```

✅ Expect: `200` JSON with metric `up`.

### 5.2 Mimir suppression (out-of-order sample)

Send the same WriteRequest twice within a short window so Mimir returns `400 out of order sample`.
✅ Expect: Gateway returns `204` (suppressed, not an error to the caller).

Check metric:

```bash
curl -s http://localhost:8080/metrics | grep suppressed
```

✅ Expect: `gateway_suppressed_errors_total{pattern="out of order sample"}` > 0.

### 5.3 Label inject on Mimir

Add `labels.inject: {region: "us-east-1"}` to the mimir instance. Restart.
Push a metric, then query it.
✅ Expect: The stored metric includes `region="us-east-1"`.

---

## 6. Fan-out Behaviour

Configure a fan-out instance (requires a second Loki or a simple HTTP echo):

```yaml
instances:
  - name: loki-fanout
    backend: loki
    fan_out_mode: any
    push_urls:
      - url: http://loki-primary:3100
      - url: http://loki-secondary:3100
```

### 6.1 `any` mode — both targets alive → clean 204

```bash
curl -i -X POST http://localhost:8080/api/loki/push \
  -H "Authorization: Bearer lab-token" \
  -H "Content-Type: application/json" \
  -d '{"streams":[{"stream":{"app":"fanout-test"},"values":[["'"$(date +%s)000000000"'","fanout ok"]]}]}'
```

✅ Expect: `204`, **no** `X-Gateway-Partial-Failure` header.

### 6.2 `any` mode — one target down → partial failure header

Stop `loki-secondary`. Repeat the push above.
✅ Expect: `204`, header `X-Gateway-Partial-Failure: target=http://loki-secondary:3100 status=502`.

Check metric:

```bash
curl -s http://localhost:8080/metrics | grep partial_failure
```

✅ Expect: `gateway_partial_failures_total{instance="loki-fanout"} 1`.

### 6.3 Partial failure counter is not double-counted

Push twice with one target down.
✅ Expect: `gateway_partial_failures_total{instance="loki-fanout"} 2` (increments by **1** per request, not per failing target).

### 6.4 `any` mode — both targets down → 502

Stop both targets. Push again.
✅ Expect: `502 Bad Gateway`, JSON body `{"error":"all push targets failed","instance":"loki-fanout"}`.

### 6.5 `all` mode — one target down → 502

Change `fan_out_mode: all`. Stop one target. Push.
✅ Expect: `502`, JSON body `{"error":"push target failed"}`.

### 6.6 Per-target auth override

```yaml
push_urls:
  - url: http://loki-a:3100
    basic_auth: "user-a:pass-a"
  - url: http://loki-b:3100
    basic_auth: "user-b:pass-b"
```

Use tcpdump or an access log on the upstream to verify each target receives its own `Authorization: Basic ...` header.

---

## 7. Tempo Traces

### 7.1 OTLP push and trace retrieval

Push a trace via otelcol or a test sender to:
```
POST http://localhost:8080/api/tempo/otlp/v1/traces
Authorization: Bearer lab-token
```

Capture the trace ID, then query back:

```bash
curl -s "http://localhost:8080/api/tempo/traces/<TRACE_ID>" \
  -H "Authorization: Bearer lab-token"
```

✅ Expect: `200` with trace JSON/protobuf data.

### 7.2 Tempo search

```bash
curl -s "http://localhost:8080/api/tempo/search?tags=service.name=my-service" \
  -H "Authorization: Bearer lab-token"
```

✅ Expect: `200` JSON search results.

### 7.3 Tag names

```bash
curl -s "http://localhost:8080/api/tempo/v2/search/tags" \
  -H "Authorization: Bearer lab-token"
```

✅ Expect: `200` JSON list of tag names.

### 7.4 Tag values

```bash
curl -s "http://localhost:8080/api/tempo/v2/search/tag/service.name/values" \
  -H "Authorization: Bearer lab-token"
```

✅ Expect: `200` JSON list of values.

---

## 8. Auth Passthrough Mode

Configure an instance:

```yaml
instances:
  - name: loki-passthrough
    backend: loki
    url: http://loki:3100
    auth_passthrough: true
```

### 8.1 Client Authorization header forwarded

```bash
curl -i -X POST http://localhost:8080/api/loki/push \
  -H "Authorization: Bearer lab-token" \
  -H "Content-Type: application/json" \
  -d '{"streams":[]}'
```

Capture the request at the Loki access log.
✅ Expect: `Authorization: Bearer lab-token` is present in the upstream request.

### 8.2 `X-Scope-OrgID` forwarded

```bash
curl -i -X POST http://localhost:8080/api/loki/push \
  -H "Authorization: Bearer lab-token" \
  -H "X-Scope-OrgID: my-tenant" \
  -H "Content-Type: application/json" \
  -d '{"streams":[]}'
```

✅ Expect: Loki receives `X-Scope-OrgID: my-tenant`.

### 8.3 `basic_auth` is NOT injected in passthrough mode

Even if `basic_auth` were set (invalid config — caught at startup), the upstream should receive only the client-provided headers. The startup validation in 1.6 already blocks this combination.

---

## 9. Timeout Behaviour

Restart the gateway with short timeouts:

```yaml
gateway:
  timeouts:
    query: 1s
    push: 1s
```

### 9.1 Slow query upstream → 504

Point instance at a server that sleeps 5 seconds. Issue a query.

```bash
curl -i "http://localhost:8080/api/loki/query_range?query={app=%22test%22}&start=1&end=2" \
  -H "Authorization: Bearer lab-token"
```

✅ Expect: `504 Gateway Timeout` within ~1 second, JSON `{"error":"upstream timeout"}`.

### 9.2 Slow push → 502

Push to an instance with a slow upstream.
✅ Expect: `502 Bad Gateway` within ~1 second (timeout counted as target failure in any-mode).

---

## 10. Prometheus Metrics Audit

After running all tests above, scrape `/metrics` and validate:

```bash
curl -s http://localhost:8080/metrics
```

| Metric | Expected |
|--------|----------|
| `gateway_fanout_requests_total{instance="loki-prod",status="204"}` | > 0 |
| `gateway_fanout_requests_total{instance="loki-prod",status="0"}` | > 0 (after connection failures) |
| `gateway_suppressed_errors_total{pattern="out of order sample"}` | > 0 (after suppression test) |
| `gateway_partial_failures_total{instance="loki-fanout"}` | > 0 (after fan-out partial failure test) |

---

## 11. Proto + Snappy Push (Loki)

### 11.1 Snappy-compressed protobuf push

Use Promtail or Grafana Alloy configured to push to the gateway in `application/x-protobuf` format.
✅ Expect: `204`, data queryable in Loki.

### 11.2 Label rewrite on proto push

With `labels.inject` configured, push via Promtail/Alloy.
Query Loki directly and verify injected labels are present on the stored streams.

---

## Summary Checklist

| # | Test | Expected result |
|---|------|-----------------|
| 1.1 | Healthy start | Gateway running |
| 1.2 | Missing token | Startup error |
| 1.3 | Invalid backend | Startup error |
| 1.4 | Duplicate instance name | Startup error |
| 1.5 | `url` + `push_urls` both set | Startup error |
| 1.6 | `auth_passthrough` + `basic_auth` | Startup error |
| 1.7 | Tempo + `push_urls` | Startup error |
| 1.8 | Tempo + `labels` | Startup error |
| 2.1 | No token | 401 |
| 2.2 | Wrong token | 401 |
| 2.3 | Correct token | 204 |
| 2.4 | `/healthz` no token | 200 |
| 2.5 | `/metrics` no token | 200 + metrics |
| 3.1 | Loki push + query | 204 / data visible |
| 3.2 | Unknown instance | 404 |
| 3.3 | Wrong backend | 404 |
| 3.4 | Loki labels endpoint | 200 |
| 3.5 | Loki series endpoint | 200 |
| 4.1 | Denylist filter + inject | Denied label absent, injected present |
| 4.2 | Allowlist filter | Only allowed + injected labels survive |
| 5.1 | Mimir remote write + query | 200 metric data |
| 5.2 | Mimir suppression | 204, suppression counter incremented |
| 5.3 | Mimir label inject | Injected label on stored metric |
| 6.1 | Fan-out both alive | 204, no partial failure header |
| 6.2 | Fan-out one down | 204 + `X-Gateway-Partial-Failure` |
| 6.3 | Partial failure counter | +1 per request (not per target) |
| 6.4 | Fan-out both down | 502 |
| 6.5 | `all` mode one down | 502 |
| 6.6 | Per-target auth | Each upstream gets its own `Authorization` |
| 7.1 | Tempo OTLP push + trace query | Trace retrievable |
| 7.2 | Tempo search | 200 results |
| 7.3 | Tempo tag names | 200 list |
| 7.4 | Tempo tag values | 200 list |
| 8.1 | Passthrough auth forwarded | Client `Authorization` reaches upstream |
| 8.2 | Passthrough `X-Scope-OrgID` | OrgID forwarded |
| 9.1 | Query timeout | 504 within timeout window |
| 9.2 | Push timeout | 502 within timeout window |
| 10 | Metrics scrape | All counters present and accurate |
| 11.1 | Proto push | 204, data in Loki |
| 11.2 | Proto push + label rewrite | Injected labels on stored streams |
