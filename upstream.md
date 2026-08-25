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
- **Operational** — cluster and process control. These are **not** tenant-scoped:
  a backend serves them outside its tenant middleware, so `X-Scope-OrgID` means
  nothing to them and the gateway sends none. A named subset is proxied under
  the `status`, `config` and `metrics` grants; the rest is listed
  so it is clear it was considered and excluded, not overlooked.

  The subset is addressed as `{mount}/targets/{instance}/{alias}`, which asks
  **every** configured target and answers with all of them, so an operator can
  see which replica of an instance disagrees with the others. It is served
  identically on the data listener and the admin listener; the admin listener's
  answer additionally names each target's URL.

  Because these endpoints answer for the cluster rather than for a tenant, they
  are not reachable on a `read` grant, and the three actions are graded by what
  the answer contains rather than by what the endpoint is called:

  | Action | Covers | Why separate |
  | --- | --- | --- |
  | `status` | liveness, module state, build info, echo | facts about the process |
  | `config` | running config, flags, runtime overrides | `runtime_config` is keyed by tenant ID, so it enumerates the cluster's tenants and their limits |
  | `metrics` | the raw `/metrics` exposition | per-tenant data in one document; **excluded from the `*` action wildcard** and grantable only by name |

  `metrics` is the one to be careful with. Every backend labels its own
  metrics by tenant — Loki's `loki_discarded_bytes_total` carries `tenant`,
  Mimir's `cortex_distributor_received_samples_total` carries `user`, and
  Tempo's `tempo_distributor_debug_spans_received_total` carries `tenant`,
  `name` and `service` together — so granting it to one tenant shows them every
  other tenant's ingest volume, error profile, and in Tempo's case a partial map
  of their services. Treat it as an operator grant.

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

Mimir's `RegisterAlertmanager` mounts the whole Alertmanager beneath
`-http.alertmanager-http-prefix` with one `RegisterRoutesWithPrefix(..., auth:
true, ...)`, which is why the v2 paths appear in no route table of its own and
why they sit behind the tenant middleware. Build info is the exception,
registered explicitly with `auth: false` ahead of the prefix handler.

What Grafana requests is transcribed from the `endpoints` map in
`pkg/services/ngalert/api/lotex_am.go`:

| Endpoint | Methods | `mimir` and `cortex` | `prometheus` |
| --- | --- | --- | --- |
| silences | GET, POST | `/alertmanager/api/v2/silences` | `/api/v2/silences` |
| silence | GET, DELETE | `/alertmanager/api/v2/silence/{id}` | `/api/v2/silence/{id}` |
| status | GET | `/alertmanager/api/v2/status` | `/api/v2/status` |
| groups | GET | `/alertmanager/api/v2/alerts/groups` | `/api/v2/alerts/groups` |
| alerts | GET, POST | `/alertmanager/api/v2/alerts` | `/api/v2/alerts` |
| config | GET, POST, DELETE | `/api/v1/alerts` | — |

The `cortex` and `mimir` entries are identical path for path, and `cortex` is
the default when a data source names no implementation, so one route set covers
Mimir, Cortex and unset alike.

Two things do **not** come from that map:

- **Feature discovery.** Before testing the data source Grafana calls
  `discoverAlertmanagerFeaturesByUrl`, which calls `fetchPromBuildInfo` — a
  helper shared with the Prometheus data source that appends
  `/api/v1/status/buildinfo` to the base URL **without** the `/alertmanager`
  prefix. So the gateway path is undoubled while the upstream path is prefixed.
  A 404 here is read as "Cortex, which has no buildinfo endpoint", which clears
  `lazyConfigInit` and makes Grafana report a *failed* health check for a Mimir
  Alertmanager that merely has no configuration for the tenant yet.
- **The `prometheus` implementation is deliberately not served**, and not only
  because Mimir does not answer that shape. Grafana's `testDatasource` probes
  `{url}/api/v2/status` **first** for a Mimir or Cortex data source and treats a
  200 as proof of misconfiguration ("detected a Prometheus endpoint"). The two
  shapes are mutually exclusive on one base URL by Grafana's own design, so this
  mount's 404 for `/api/v2/status` is load-bearing.

The gateway serves this as a fourth data source mount, whose job is to map
Grafana's Alertmanager data source patterns onto Mimir. It is a URL shape rather
than a system: the instances behind it are Mimir instances, so it registers no
`targets/{instance}/{alias}` routes of its own — those would reach the same
targets, and return the same answers, as the ones under `/prometheus`.

Mimir's Alertmanager-specific operational endpoints are the
`/multitenant_alertmanager/*` family, and they are **not proxied at any grant**.
`/multitenant_alertmanager/configs` is registered `auth=false`, and its handler
lists every tenant in the store and streams each one's full Alertmanager
configuration — which is where receiver credentials live: Slack webhook URLs,
PagerDuty routing keys, SMTP passwords. That is a larger disclosure than the raw
`/metrics` the `metrics` action gates, with no consumer-facing use to weigh
against it. `/multitenant_alertmanager/{status,ring}` are ring topology and
tenant enumeration and are excluded with it.

### Operational — proxied under the operational grants

Mimir registers each of these with `auth=false`, which is what decides whether
its tenant middleware wraps the handler, so none of them reads `X-Scope-OrgID`
and the gateway sends none.

| Alias | Upstream | Action |
| --- | --- | --- |
| `ready` | `/ready` | `status` |
| `services` | `/services` | `status` |
| `buildinfo` | `/api/v1/status/buildinfo` | `status` |
| `config` | `/config` | `config` |
| `status_config` | `/api/v1/status/config` | `config` |
| `flags` | `/api/v1/status/flags` | `config` |
| `runtime_config` | `/runtime_config` | `config` |
| `metrics` | `/metrics` | `metrics` |

Mimir's own index page describes `/runtime_config` as "Entire runtime config
(including overrides)" — the per-tenant limits map — which is why it is
`config` and not `status`.

Three of these are **also** still served at their Mimir paths under the
`/prometheus` mount, from before the operational grants existed. Only the two
configuration dumps moved: `/prometheus/api/v1/status/{config,flags}` now
require `config`, **and are forwarded operationally** — no tenant header, no
read counters, and no entry in the read cool-off, so a failing configuration
dump cannot park a healthy replica and degrade query traffic.

Their observable behaviour is unchanged: same passthrough response, same
response headers, and the same retry rule (a transport failure or a 5xx moves on
to the next target, a 4xx is relayed). The one difference is that the body is
collected rather than streamed and therefore capped at 1 MiB, with an oversize
response refused rather than truncated.

`/prometheus/ready` and `/prometheus/api/v1/status/buildinfo` stay on `read` —
they are what a client calls to decide whether the backend is usable, and
Grafana's Prometheus data source reads build info itself for feature detection,
so reclassifying either would break every existing read-only data source. The
Alertmanager mount's `/alertmanager/api/v1/status/buildinfo` is the same case.

They are forwarded as health checks rather than as queries. That changes two
things and nothing else. The instance is the first one configured for the
backend, chosen without reference to the caller's tenants, because a liveness
probe has no tenant to match on and matching anyway made a tenant-dedicated
instance answer `404 no matching instance` — or, with two dedicated instances,
`409 ambiguous backend instances` — to a caller whose grant did not line up. And
no `X-Scope-OrgID` is sent, for the reason the table above gives: these
endpoints are not registered behind the tenant middleware. Target order, retry
rule, failover and read cool-off are the ordinary read path's.

### Operational — not proxied

`GET /`, `/memberlist`, `/debug/pprof/*`, `/debug/fgprof`,
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

`/loki/api/v1/tail` upgrades to a WebSocket, so it takes a separate forwarding
path in the gateway that hijacks the connection rather than buffering a
response. It is single-tenant: Loki answers 400 when more than one tenant is
supplied.

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

Destructive and tenant-scoped, so these sit behind a `delete` action rather than
the ordinary `write` grant -- a log shipper needs `write` to ship and must not
thereby be able to delete what it shipped. One action covers the whole API:
listing, requesting and cancelling a deletion are the same privilege. A `delete`
grant is always single-tenant, because deletion is irreversible and a caller who
cannot say which tenant they mean must not delete from any.

Mimir has no equivalent API -- it cannot delete individual series -- so `delete`
is not a valid action on the `mimir` backend. See the FUTURE entry in
DEFECTS.md.

### Deprecated

`POST /api/prom/push`, `GET /api/prom/tail`, `GET /api/prom/query`,
`GET /api/prom/label`, `GET /api/prom/label/{name}/values`, `GET /api/prom/series`.

### Operational — proxied under the operational grants

| Alias | Upstream | Action |
| --- | --- | --- |
| `ready` | `/ready` | `status` |
| `services` | `/services` | `status` |
| `buildinfo` | `/loki/api/v1/status/buildinfo` | `status` |
| `config` | `/config` | `config` |
| `log_level` | `/log_level` | `config` |
| `metrics` | `/metrics` | `metrics` |

`log_level` is **GET only**. dskit serves POST on the same path to change the
running log level, and the registration table has no way to ask for a method
other than GET, so a config grant cannot become a process control.

Loki's `/config` masks fields typed `flagext.Secret` (rendering them
`********`) but not adjacent plain-string fields such as `access_key_id`, and it
carries the runtime overrides section, so it is `config`.

`/loki/api/v1/status/buildinfo` also remains reachable at its Loki path under
the `/loki` mount on a plain `read` grant, because Grafana's Loki data source
reads it for feature detection. It is forwarded there as a health check: first
configured Loki instance regardless of the caller's tenants, first working
target within it, and no `X-Scope-OrgID` — see the Mimir section for why.

### Operational — not proxied

`/distributor/ring`, `/indexgateway/ring`, `/ruler/ring`, `/compactor/ring`,
`POST /flush`, `/ingester/prepare_shutdown` (GET, POST, DELETE),
`/ingester/shutdown` (GET, POST), `POST /log_level`.

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

Ingest runs on **separate receiver ports** from the query API. Express this by
giving the instance grouped targets: `group: query` for the HTTP API and
receiver groups such as `group: otlp_http`, `group: jaeger` and `group: zipkin`
for the matching HTTP ingest routes; see the OTLP exporters section of the
README. The receiver groups exist **only for Tempo**: Mimir and Loki serve OTLP
HTTP on the same distributor listener as their other ingest paths, so the
gateway always routes their ingest to `group: push` and rejects a target on
either backend that names a receiver group it does not serve.

Tenancy is not a target property. `tenant_id` belongs to the instance and every
target asserts it, because an instance's targets address one backend -- with
groups, often two surfaces of one process -- where two tenants would mean
writing as one and reading as the other. Two tenants means two instances.

A target with no group is a legacy fallback for every HTTP surface, and reads
fall back to generic push or ungrouped targets when an instance declares no
query group — so this changes nothing for a configuration written before groups
existed.

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

### Operational — proxied under the operational grants

| Alias | Upstream | Action |
| --- | --- | --- |
| `ready` | `/ready` | `status` |
| `status` | `/status` | `status` |
| `buildinfo` | `/api/status/buildinfo` | `status` |
| `echo` | `/api/echo` | `status` |
| `config` | `/status/config` | `config` |
| `runtime_config` | `/status/runtime_config` | `config` |
| `metrics` | `/metrics` | `metrics` |

Tempo registers `/api/echo` with a bare handler while its query routes go
through `base.Wrap(...)`, which is where the tenant middleware lives — so echo,
like the rest of this table, is untenanted upstream.

`/api/echo` also remains reachable at its Tempo path under the `/tempo` mount on
a plain `read` grant, together with `/api/status/buildinfo`, because `/api/echo`
is what Grafana's Tempo data source uses for its health check. Both are
forwarded as health checks: first configured Tempo instance regardless of the
caller's tenants, first working target within it, and no `X-Scope-OrgID` — see
the Mimir section for why. Routing echo by tenant is what produced "Tempo echo
endpoint returned status 404" against a tenant-dedicated Tempo whose tenant the
data source's grant did not name.

### Operational — not proxied

`GET /debug/pprof`, `/status/backendscheduler`, `/usage_metrics`, `/memberlist`,
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
| `GET /rules[/{ns}[/{grp}]]` | `/prometheus/rules...` | `/prometheus/config/v1/rules...` |
| `POST /rules/{ns}` | `/prometheus/rules/{ns}` | `/prometheus/config/v1/rules/{ns}` |
| `DELETE /rules/{ns}[/{grp}]` | `/prometheus/rules...` | `/prometheus/config/v1/rules...` |

The `/rules` rows are Grafana's other spelling of the ruler configuration API,
used when the data source subtype is `cortex`, `prometheus`, or absent. Mimir
serves only the `/config/v1` form, so these six are an alias rather than a
passthrough -- the one place the gateway does not forward mount plus the exact
upstream path.

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
| `GET /loki/api/v1/tail` | `/loki/loki/api/v1/tail` | `/loki/api/v1/tail` (WebSocket) |
| `GET,POST,PUT,DELETE /loki/api/v1/delete` | `/loki/loki/api/v1/delete` | `/loki/api/v1/delete` |

The `/loki/loki` doubling is correct: the mount is a gateway concept and
`/loki/api/v1` is genuinely part of Loki's own paths. Under the mount, Loki's
rule and alert state no longer competes with Mimir's identical paths at the
gateway root.

**Status: complete.**

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
| Loki | `gateway:port/loki` | complete |
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
