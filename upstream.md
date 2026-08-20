# Upstream API Shapes

Reference for the HTTP surface of the three backends this gateway fronts:
**Mimir** (metrics), **Loki** (logs) and **Tempo** (traces).

It exists because the gateway's own routing decisions depend on details that are
easy to get wrong from memory: which paths each project actually serves, which
of them collide between projects, and which are safe to expose to a tenant.

Every route below is transcribed from the projects' own HTTP API references.
Where a path comes from somewhere else, it is marked.

Sources:

- <https://grafana.com/docs/mimir/latest/references/http-api/>
- <https://grafana.com/docs/loki/latest/reference/loki-http-api/>
- <https://grafana.com/docs/tempo/latest/api_docs/>

## Reading this document

Routes are grouped by the concern they serve, because the gateway treats those
concerns separately:

- **Ingestion** — a client is configured with a *complete* URL. The gateway
  mirrors these paths exactly.
- **Data source** — Grafana is configured with a *base* URL and appends paths of
  its own choosing. The gateway controls only the base.
- **Operational** — cluster and process control. These are **not** tenant-scoped
  and must not be proxied to tenants; they are listed so it is clear they were
  considered and excluded, not overlooked.

Two configurable prefixes appear throughout:

| Placeholder | Default | Configured by |
| --- | --- | --- |
| `<prometheus-http-prefix>` | `/prometheus` | Mimir `-http.prometheus-http-prefix` |
| `<alertmanager-http-prefix>` | `/alertmanager` | Mimir `-http.alertmanager-http-prefix` |

This document assumes the defaults.

---

## Mimir

### Ingestion

| Method | Path | Protocol |
| --- | --- | --- |
| POST | `/api/v1/push` | Prometheus remote write |
| POST | `/otlp/v1/metrics` | OTLP HTTP |
| POST | `/api/v1/push/influx/write` | InfluxDB line protocol |

Note these sit at the **root**, not under `/prometheus`. Mimir's ingestion paths
are siblings of its query prefix, not children of it.

### Data source (Prometheus-compatible query API)

All under `/prometheus`.

| Method | Path |
| --- | --- |
| GET, POST | `/prometheus/api/v1/query` |
| GET, POST | `/prometheus/api/v1/query_range` |
| GET, POST | `/prometheus/api/v1/query_exemplars` |
| GET, POST | `/prometheus/api/v1/series` |
| GET, POST | `/prometheus/api/v1/labels` |
| GET | `/prometheus/api/v1/label/{name}/values` |
| GET | `/prometheus/api/v1/metadata` |
| POST | `/prometheus/api/v1/read` |
| GET, POST | `/prometheus/api/v1/format_query` |
| GET | `/prometheus/api/v1/status/buildinfo` |
| GET, POST | `/prometheus/api/v1/cardinality/active_series` |
| GET, POST | `/prometheus/api/v1/cardinality/label_names` |
| GET, POST | `/prometheus/api/v1/cardinality/label_values` |
| GET, POST | `/prometheus/api/v1/search/metric_names` (experimental) |
| GET, POST | `/prometheus/api/v1/search/label_names` (experimental) |
| GET, POST | `/prometheus/api/v1/search/label_values` (experimental) |

The POST variants are reads: PromQL selectors can outgrow a practical URL, so
these accept form-encoded bodies.

### Ruler

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/prometheus/api/v1/rules` | Rule and alert state |
| GET | `/prometheus/api/v1/alerts` | Firing alerts |
| GET | `/prometheus/config/v1/rules` | List rule groups |
| GET | `/prometheus/config/v1/rules/{namespace}` | Groups in a namespace |
| GET | `/prometheus/config/v1/rules/{namespace}/{groupName}` | One group |
| POST | `/prometheus/config/v1/rules/{namespace}` | Create or update a group |
| DELETE | `/prometheus/config/v1/rules/{namespace}/{groupName}` | Delete a group |
| DELETE | `/prometheus/config/v1/rules/{namespace}` | Delete a namespace |

Mimir's ruler supports **federated rule groups**: a group carrying
`source_tenants` is evaluated against several tenants at once. This is why a
multi-tenant `rules:read` grant is meaningful for Mimir and not for Loki.

### Alertmanager

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/v1/alerts` | Get tenant Alertmanager configuration |
| POST | `/api/v1/alerts` | Set tenant Alertmanager configuration |
| DELETE | `/api/v1/alerts` | Delete tenant Alertmanager configuration |
| GET | `/alertmanager` | Alertmanager UI |
| GET | `/alertmanager/api/v1/status/buildinfo` | Build info |

The configuration API is at the **root**, not under `/prometheus`.

### Operational — not proxied

`GET /`, `/config`, `/runtime_config`, `/services`, `/ready`, `/metrics`,
`/memberlist`, `/debug/pprof/*`, `/debug/fgprof`,
`/api/v1/status/config`, `/api/v1/status/flags`, `/api/v1/status/buildinfo`,
`/api/v1/user_limits`, `/api/v1/user_stats`,
`/distributor/{ring,ha_tracker,all_user_stats}`,
`/ingester/*` (flush, shutdown, downscale, ring, tenants, tsdb),
`/query-scheduler/ring`, `/ruler/{ring,rule_groups,delete_tenant_config}`,
`/multitenant_alertmanager/*`, `/store-gateway/*`, `/compactor/*`,
`/api/v1/upload/block/*`, `/overrides-exporter/ring`.

Several of these are destructive (`/ingester/shutdown`, `/compactor/delete_tenant`)
or leak cross-tenant information (`/distributor/all_user_stats`).

---

## Loki

### Ingestion

| Method | Path | Protocol |
| --- | --- | --- |
| POST | `/loki/api/v1/push` | Loki push (JSON or snappy protobuf) |
| POST | `/otlp/v1/logs` | OTLP HTTP |
| POST | `/api/prom/push` | Deprecated legacy push |

### Data source (query API)

| Method | Path |
| --- | --- |
| GET | `/loki/api/v1/query` |
| GET | `/loki/api/v1/query_range` |
| GET | `/loki/api/v1/labels` |
| GET | `/loki/api/v1/label/{name}/values` |
| GET, POST | `/loki/api/v1/series` |
| GET | `/loki/api/v1/index/stats` |
| GET | `/loki/api/v1/index/volume` |
| GET | `/loki/api/v1/index/volume_range` |
| GET | `/loki/api/v1/patterns` |
| GET, POST | `/loki/api/v1/detected_fields` |
| GET, POST | `/loki/api/v1/detected_field/{name}/values` |
| GET, POST | `/loki/api/v1/format_query` |
| GET | `/loki/api/v1/status/buildinfo` |
| GET | `/loki/api/v1/tail` | **WebSocket** |

`/loki/api/v1/tail` upgrades to a WebSocket and cannot be carried by an ordinary
buffering HTTP proxy.

### Ruler

Loki serves its ruler configuration API under two spellings, plus a
Prometheus-compatible read-only pair.

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/loki/api/v1/rules` | List rule groups |
| GET | `/loki/api/v1/rules/{namespace}` | Groups in a namespace |
| GET | `/loki/api/v1/rules/{namespace}/{groupName}` | One group |
| POST | `/loki/api/v1/rules/{namespace}` | Create or update a group |
| DELETE | `/loki/api/v1/rules/{namespace}/{groupName}` | Delete a group |
| DELETE | `/loki/api/v1/rules/{namespace}` | Delete a namespace |
| GET/POST/DELETE | `/api/prom/rules...` | Legacy spelling of the six above |
| GET | `/prometheus/api/v1/rules` | Rule and alert state |
| GET | `/prometheus/api/v1/alerts` | Firing alerts |

Loki has a ruler but **no Alertmanager** — it forwards firing alerts to an
external one. There is therefore no alert-configuration write API.

Loki's ruler does **not** support federated rules; `source_tenants` is an open
feature request (grafana/loki#7659). Rule endpoints are single-tenant only.

### Log deletion

| Method | Path |
| --- | --- |
| POST, PUT | `/loki/api/v1/delete` |
| GET | `/loki/api/v1/delete` |
| DELETE | `/loki/api/v1/delete` |

Destructive and tenant-scoped. Exposing these under an ordinary `write` grant
would let any log shipper delete logs, so they warrant a distinct permission.

### Deprecated

`POST /api/prom/push`, `GET /api/prom/tail`, `GET /api/prom/query`,
`GET /api/prom/label`, `GET /api/prom/label/{name}/values`, `GET /api/prom/series`.

### Operational — not proxied

`GET /ready`, `/log_level` (GET, POST), `/metrics`, `/config`, `/services`,
`/distributor/ring`, `/indexgateway/ring`, `/ruler/ring`, `/compactor/ring`,
`POST /flush`, `/ingester/prepare_shutdown` (GET, POST, DELETE),
`/ingester/shutdown` (GET, POST).

### Multi-tenancy

Loki accepts a pipe-joined `X-Scope-OrgID` (`tenant-a|tenant-b`), but **only on
query endpoints**. The documentation is explicit that
`GET /loki/api/v1/tail` and `POST /loki/api/v1/push` return HTTP 400 when more
than one tenant is supplied. Ruler endpoints are likewise single-tenant.

---

## Tempo

### Ingestion

Tempo's distributor is built on OpenTelemetry Collector receivers. Its HTTP API
reference does **not** document ingest paths; it defers to the receivers, whose
paths and ports come from receiver configuration rather than from Tempo itself.

| Method | Path | Protocol | Default port |
| --- | --- | --- | --- |
| POST | `/v1/traces` | OTLP HTTP | 4318 |
| — | — | OTLP gRPC | 4317 |
| POST | `/api/traces` | Jaeger Thrift HTTP | 14268 |
| POST | `/api/v2/spans` | Zipkin | 9411 |

**These four rows are OpenTelemetry Collector receiver defaults, not values
transcribed from Tempo's API reference.** Confirm them against the deployment's
receiver configuration before relying on them. Note also that Tempo serves a
bare `/v1/traces`, without the `/otlp` prefix that Mimir and Loki use.

Ingest runs on **separate receiver ports** from the query API, which a gateway
reaching an instance through a single configured URL cannot express.

### Data source (query API)

Served by the query-frontend.

| Method | Path |
| --- | --- |
| GET | `/api/traces/{traceID}` |
| GET | `/api/v2/traces/{traceID}` |
| GET | `/api/search` |
| GET | `/api/search/tags` |
| GET | `/api/v2/search/tags` |
| GET | `/api/search/tag/{tag}/values` |
| GET | `/api/v2/search/tag/{tag}/values` |
| GET | `/api/metrics/query_range` |
| GET | `/api/metrics/query` |
| GET | `/api/echo` |
| GET | `/api/status/buildinfo` |

### Overrides

| Method | Path |
| --- | --- |
| GET, POST, PATCH, DELETE | `/api/overrides` |

### Operational — not proxied

`GET /ready`, `/metrics`, `/debug/pprof`, `/status`,
`/status/backendscheduler`, `/usage_metrics`, `/memberlist`,
`/distributor/ring`, `/live-store/ring`, `/partition-ring`,
`/live-store/prepare-partition-downscale` (GET, POST, DELETE),
`/live-store/prepare-downscale` (GET, POST, DELETE).

---

## Cross-project collisions

Paths more than one project serves. Any gateway hosting several backends in one
URL space has to resolve these.

| Path | Served by | Note |
| --- | --- | --- |
| `/prometheus/api/v1/rules` | Mimir ruler, Loki ruler | Both are rule state |
| `/prometheus/api/v1/alerts` | Mimir ruler, Loki ruler | Both are firing alerts |
| `/api/prom/push` | Loki legacy, Mimir Cortex-compat | Both deprecated |
| `/api/prom/rules...` | Loki legacy ruler, Mimir Cortex-compat | |
| `/ready`, `/metrics`, `/config`, `/services` | all three | Operational |
| `/distributor/ring` | Mimir, Tempo | Operational |
| `/memberlist` | Mimir, Tempo | Operational |

The ingestion paths do **not** collide with one another, with the sole exception
of the deprecated `/api/prom/push`. That is what makes it possible to mirror all
three projects' ingestion surfaces in one flat namespace.

`GET /api/traces/{traceID}` (Tempo query) and `POST /api/traces` (Jaeger ingest)
share a prefix but differ in method and shape, so they are separable.

---

## Data source mapping

Three columns, because three different things have to line up:

- **Data source API** — what Grafana appends to the configured base URL. Not
  negotiable; Grafana decides it.
- **Our shape** — what the gateway serves today.
- **Upstream shape** — what the backend serves.

A row works only when a single base URL turns column 1 into column 2.

### Mimir — base `gateway:port/prometheus`

| Data source API | Our shape | Upstream shape |
| --- | --- | --- |
| `GET,POST /api/v1/query` | `/prometheus/api/v1/query` | `/prometheus/api/v1/query` |
| `GET,POST /api/v1/query_range` | `/prometheus/api/v1/query_range` | same |
| `GET,POST /api/v1/query_exemplars` | `/prometheus/api/v1/query_exemplars` | same |
| `GET,POST /api/v1/series` | `/prometheus/api/v1/series` | same |
| `GET,POST /api/v1/labels` | `/prometheus/api/v1/labels` | same |
| `GET,POST /api/v1/label/{name}/values` | `/prometheus/api/v1/label/{name}/values` | same |
| `GET,POST /api/v1/metadata` | `/prometheus/api/v1/metadata` | same |
| `POST /api/v1/read` | `/prometheus/api/v1/read` | same |
| `GET,POST /api/v1/format_query` | `/prometheus/api/v1/format_query` | same |
| `GET /api/v1/status/buildinfo` | `/prometheus/api/v1/status/buildinfo` | same |
| `GET,POST /api/v1/cardinality/*` | `/prometheus/api/v1/cardinality/*` | same |
| `GET /api/v1/rules` | `/prometheus/api/v1/rules` | same |
| `GET /api/v1/alerts` | `/prometheus/api/v1/alerts` | same |
| `GET /config/v1/rules[/{ns}[/{grp}]]` | `/prometheus/config/v1/rules...` | same |
| `POST /config/v1/rules/{ns}` | `/prometheus/config/v1/rules/{ns}` | same |
| `DELETE /config/v1/rules/{ns}[/{grp}]` | `/prometheus/config/v1/rules...` | same |

**Status: complete and identity-mapped.** `/prometheus` is simultaneously the
base the user types and Mimir's own API prefix, so nothing is rewritten.

### Loki — base `gateway:port/loki`

| Data source API | Our shape | Upstream shape |
| --- | --- | --- |
| `GET /loki/api/v1/query` | `/loki/loki/api/v1/query` | `/loki/api/v1/query` |
| `GET /loki/api/v1/query_range` | `/loki/loki/api/v1/query_range` | `/loki/api/v1/query_range` |
| `GET /loki/api/v1/labels` | `/loki/loki/api/v1/labels` | `/loki/api/v1/labels` |
| `GET /loki/api/v1/label/{name}/values` | `/loki/loki/api/v1/label/{name}/values` | same minus mount |
| `GET,POST /loki/api/v1/series` | `/loki/loki/api/v1/series` | same minus mount |
| `GET /loki/api/v1/index/{stats,volume,volume_range}` | `/loki/loki/api/v1/index/*` | same minus mount |
| `GET /loki/api/v1/patterns` | `/loki/loki/api/v1/patterns` | same minus mount |
| `GET,POST /loki/api/v1/detected_fields` | `/loki/loki/api/v1/detected_fields` | same minus mount |
| `GET,POST /loki/api/v1/detected_field/{name}/values` | `/loki/loki/api/v1/detected_field/...` | same minus mount |
| `GET,POST /loki/api/v1/format_query` | `/loki/loki/api/v1/format_query` | same minus mount |
| `GET /loki/api/v1/status/buildinfo` | `/loki/loki/api/v1/status/buildinfo` | same minus mount |
| `GET /loki/api/v1/rules...` | `/loki/loki/api/v1/rules...` | same minus mount |
| `GET,POST,DELETE /api/prom/rules...` | `/loki/api/prom/rules...` | `/api/prom/rules...` |
| `GET /prometheus/api/v1/rules` | `/loki/prometheus/api/v1/rules` | `/prometheus/api/v1/rules` |
| `GET /prometheus/api/v1/alerts` | `/loki/prometheus/api/v1/alerts` | `/prometheus/api/v1/alerts` |
| `GET /loki/api/v1/tail` | — **not implemented** | WebSocket |

The `/loki/loki` doubling is correct: the mount is a gateway concept and
`/loki/api/v1` is genuinely part of Loki's own paths. Under the mount, Loki's
rule and alert state no longer competes with Mimir's identical paths at the
gateway root.

**Status: complete except live tail.**

### Tempo — base `gateway:port/tempo`

| Data source API | Our shape | Upstream shape |
| --- | --- | --- |
| `GET /api/traces/{traceID}` | `/tempo/api/traces/{traceID}` | `/api/traces/{traceID}` |
| `GET /api/v2/traces/{traceID}` | `/tempo/api/v2/traces/{traceID}` | `/api/v2/traces/{traceID}` |
| `GET /api/search` | `/tempo/api/search` | `/api/search` |
| `GET /api/search/tags` | `/tempo/api/search/tags` | `/api/search/tags` |
| `GET /api/v2/search/tags` | `/tempo/api/v2/search/tags` | `/api/v2/search/tags` |
| `GET /api/search/tag/{tag}/values` | `/tempo/api/search/tag/{name}/values` | `/api/search/tag/{tag}/values` |
| `GET /api/v2/search/tag/{tag}/values` | `/tempo/api/v2/search/tag/{name}/values` | `/api/v2/search/tag/{tag}/values` |
| `GET /api/metrics/query_range` | `/tempo/api/metrics/query_range` | `/api/metrics/query_range` |
| `GET /api/metrics/query` | `/tempo/api/metrics/query` | `/api/metrics/query` |
| `GET /api/echo` | `/tempo/api/echo` | `/api/echo` |
| `GET /api/status/buildinfo` | `/tempo/api/status/buildinfo` | `/api/status/buildinfo` |
| `GET,POST,PATCH,DELETE /api/overrides` | `/tempo/api/overrides` | `/api/overrides` |

**Status: complete.**

### Alertmanager — base `gateway:port/alertmanager`

Grafana's Alertmanager data source with implementation set to Mimir. Paths
transcribed from Grafana's `lotex_am.go`.

| Data source API | Our shape | Upstream shape |
| --- | --- | --- |
| `GET,POST /alertmanager/api/v2/silences` | `/alertmanager/alertmanager/api/v2/silences` | `/alertmanager/api/v2/silences` |
| `GET,DELETE /alertmanager/api/v2/silence/{id}` | `/alertmanager/alertmanager/api/v2/silence/{id}` | `/alertmanager/api/v2/silence/{id}` |
| `GET /alertmanager/api/v2/status` | `/alertmanager/alertmanager/api/v2/status` | `/alertmanager/api/v2/status` |
| `GET /alertmanager/api/v2/alerts/groups` | `/alertmanager/alertmanager/api/v2/alerts/groups` | `/alertmanager/api/v2/alerts/groups` |
| `GET,POST /alertmanager/api/v2/alerts` | `/alertmanager/alertmanager/api/v2/alerts` | `/alertmanager/api/v2/alerts` |
| `GET,POST,DELETE /api/v1/alerts` | `/alertmanager/api/v1/alerts` | `/api/v1/alerts` |

One mount carries two upstream shapes, because Mimir serves the v2 API under
its `/alertmanager` prefix but the tenant configuration endpoint at its root.
Every route is single-tenant: Mimir's Alertmanager has no tenant federation.

**Status: complete.** The web UI and the `/multitenant_alertmanager/*` operator
endpoints are deliberately excluded.

### Summary

| Backend | Base URL | State |
| --- | --- | --- |
| Mimir | `gateway:port/prometheus` | complete, identity-mapped |
| Alertmanager | `gateway:port/alertmanager` | complete |
| Loki | `gateway:port/loki` | complete except live tail (WebSocket) |
| Tempo | `gateway:port/tempo` | complete |

Each mount serves its backend's exact upstream layout, and the gateway strips
the mount before forwarding. Mimir is the exception that proves the rule: its
mount and its upstream prefix are the same string, `/prometheus`, so nothing is
rewritten at all.

The older flattened `/api/{backend}/...` shape has been removed: no Grafana base
URL could produce it, and ingestion now has its own upstream-exact namespace, so
nothing addressed it. The one exception is Mimir's Alertmanager configuration
API, which Mimir serves at its root rather than under the Prometheus prefix and
which therefore has no home beneath the `/prometheus` mount. It stays at
`/api/mimir/api/v1/alerts` until the Grafana Alertmanager data source is given
the same mapping treatment as the other three.
