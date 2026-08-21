# Observability Gateway

The obsgateway is a Go-based observability data-layer gateway. It sits in front of Loki (logs), Mimir (metrics), and Tempo (traces) and exposes a single HTTP API that proxies/rewrites push and query traffic, injects tenant headers, and can fan writes out to multiple backends.

## Using

### LOKI

Loki needs to be configured to enable multitenancy. This is done by setting
`auth_enabled` to true.

```yaml
auth_enabled: true
querier:
  multi_tenant_queries_enabled: true
```

In the helm chart values:

```yaml
loki:
  config: |
    auth_enabled: true
    querier:
      multi_tenant_queries_enabled: true
```

Live tail (`GET /loki/api/v1/tail`) is a separate `tail` grant rather than part
of `read`, because Loki serves a tail for one tenant only: it answers 400 when
`X-Scope-OrgID` names more than one. A read grant may cover several tenants and
there is nothing in the request to pick one of them, so the choice is made in
the grant instead. A `tail` grant carries exactly one tenant ID, and a user with
no `tail` grant gets a 403 from the gateway rather than a 400 from Loki.

A tail never sees more than the same user's `read` grant allows: when a `read`
grant for that tenant carries a read label policy, it is applied to the tail as
well. Set the same selector on the `tail` grant or leave the tail's blank —
a `tail` policy that disagrees with the `read` policy is refused, because only
one selector can be sent upstream.


### MIMIR

Mimir needs to be configured to enable multitenancy, especially for queries. 

In the helm chart you can add:

```yaml
mimir:
  structuredConfig:
    multitenancy_enabled: true
    tenant_federation:
      enabled: true
    ruler:
      tenant_federation:
        enabled: true
``` 

or you can modify the mimir.yaml

```yaml
multitenancy_enabled: true
tenant_federation:
  enabled: true
ruler:
  tenant_federation:
    enabled: true
```

## Ingestion Routes

Ingestion and serving a Grafana data source are separate concerns with separate
URL namespaces. They must not be conflated:

- A **data source** is configured with a *base* URL. Grafana appends paths of its
  own choosing to it, so the gateway controls only the base.
- An **ingestion client** is configured with a *complete* URL, typed once into an
  Alloy, Promtail, Prometheus or OTLP exporter config. The gateway controls the
  whole path.

Because the whole path is ours, every ingestion route mirrors its upstream
project exactly. The gateway is addressable as if it were Mimir, Loki or Tempo
itself, so an existing shipper config works by changing only the host and port.

| Signal | Client | Gateway URL | Upstream |
| --- | --- | --- | --- |
| Metrics | Prometheus / Alloy `remote_write` | `POST /api/v1/push` | Mimir `/api/v1/push` |
| Metrics | Influx line protocol | `POST /api/v1/push/influx/write` | Mimir `/api/v1/push/influx/write` |
| Metrics | OTLP HTTP | `POST /otlp/v1/metrics` | Mimir `/otlp/v1/metrics` |
| Logs | Promtail / Alloy `loki.write` | `POST /loki/api/v1/push` | Loki `/loki/api/v1/push` |
| Logs | OTLP HTTP | `POST /otlp/v1/logs` | Loki `/otlp/v1/logs` |
| Traces | OTLP HTTP | `POST /v1/traces` | Tempo `/v1/traces` |
| Traces | Jaeger Thrift HTTP | `POST /api/traces` | Tempo `/api/traces` |
| Traces | Zipkin | `POST /api/v2/spans` | Tempo `/api/v2/spans` |

These paths do not collide with one another. The one upstream path both Loki and
Mimir serve, the deprecated Cortex-compatibility `POST /api/prom/push`, is
deliberately not exposed; use `/loki/api/v1/push` or `/api/v1/push` instead.

All ingestion routes are registered together in `RegisterIngest`
(`internal/fanout/ingest.go`), and the path-to-backend mapping is explicit rather
than parsed from the path, so a new route cannot silently acquire a backend.

### Authentication and tenancy

Every route requires credentials for a gateway user holding a `write`
grant on the matching backend. Either HTTP Basic, or an API key as
`Authorization: Bearer <key>` — see below. A `read` grant does not authorize ingestion. The
gateway sets `X-Scope-OrgID` from that user's grant; any tenant header supplied
by the client is discarded.

### OTLP exporters

Mimir and Loki namespace their OTLP endpoints under `/otlp`, while Tempo serves
a bare OTel receiver at `/v1/traces`. A single `OTEL_EXPORTER_OTLP_ENDPOINT`
therefore cannot reach all three, because the exporter appends `/v1/<signal>` to
it. Set the per-signal variables instead, which the OTLP specification treats as
complete URLs:

```bash
OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=https://gateway:8080/otlp/v1/metrics
OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=https://gateway:8080/otlp/v1/logs
OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://gateway:8080/v1/traces
```

Note that Tempo serves OTLP on a dedicated receiver port (4318 by default),
separate from its query API port. The gateway currently reaches an instance
through a single configured URL, so a Tempo instance fronted for both ingestion
and querying needs those two ports reconciled upstream.

## Building

### Using PowerShell (Windows)

If you encounter a PowerShell execution policy error when running `.\build.ps1`, you can resolve it with one of these methods:

**Option 1: Temporary (Current Session Only)**
```powershell
Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass
.\build.ps1
```

**Option 2: Unblock the Specific File**
```powershell
Unblock-File .\build.ps1
.\build.ps1
```

**Option 3: Current User (Persistent)**
```powershell
Set-ExecutionPolicy -Scope CurrentUser -ExecutionPolicy RemoteSigned
.\build.ps1
```

**Option 4: Run with Bypass Flag**
```powershell
powershell -ExecutionPolicy Bypass -File .\build.ps1
```

**Option 5: Use the Bash Script**
```powershell
bash build.sh
```

### Build Options

The build script accepts the following parameters:

```powershell
.\build.ps1 [-SkipRpm] [-SkipContainer] [-SkipUi]
```

- `-SkipRpm`: Skip RPM package building
- `-SkipContainer`: Skip container image building
- `-SkipUi`: Skip UI building (reuse existing bundle)

### Build Process

The build process:
1. Builds the admin UI (Vue.js application)
2. Compiles the Go binary for Linux AMD64
3. Generates SBOM (Software Bill of Materials) using Syft
4. Runs vulnerability scanning using Grype
5. Optionally builds RPM package
6. Optionally builds container image

## Configuration

The runtime source of truth is the configured database (SQLite or PostgreSQL). The file passed to `-config` is a minimal *bootstrap* file that opens the DB, sets listener ports, and may contain an optional `seed` block to populate a fresh database on first startup.

### Credential encryption

Upstream backend credentials (`basic_auth` on an instance or a push target) are
encrypted at rest with AES-256-GCM. They cannot be hashed the way gateway user
passwords are, because the gateway replays them on every proxied request, so
they are encrypted instead with a key held outside the database.

Generate a key:

```bash
obsgateway --generate-encryption-key --encryption-key-file /etc/obsgateway/enc.key
```

Then point the bootstrap file at it:

```yaml
gateway:
  encryption_key_file: /etc/obsgateway/enc.key
```

The key file must be mode `0600`; a group- or world-readable key file is
refused at startup. `OBSGATEWAY_ENCRYPTION_KEY` takes precedence over the file
path and carries the base64 key directly, which suits containers injecting it
from a secret store.

Behaviour at startup:

| Stored credentials | Key configured | Result |
| --- | --- | --- |
| none | no | starts normally |
| plaintext (pre-upgrade) | no | **refuses to start**, naming the fix |
| plaintext (pre-upgrade) | yes | encrypted in place, once, and logged |
| encrypted | yes, correct | starts normally |
| encrypted | no, or wrong key | **refuses to start** |

Back the key up. Without it the stored credentials cannot be recovered, and the
gateway will not start against a database it cannot read.

### Authentication throttling

Checking a password is deliberately expensive — an unknown username is compared
against a dummy hash so that a wrong username costs the same as a wrong
password, which is what stops usernames being enumerated by timing. The cost is
that a rejected credential burns real CPU for a caller who has proved nothing.

Two limits bound that, and they answer different threats:

- **Per-source throttle.** A source that fails repeatedly is blocked, with
  exponential backoff and a `Retry-After`. Blocks are per listener, so a flood
  against the data plane cannot lock an operator out of the admin API.
- **Hashing cap.** A process-wide limit on concurrent password hashes, defaulting
  to half the available CPUs so the rest stay free to serve telemetry. A request
  that cannot get a slot within `auth_hash_wait` is shed with 503 rather than
  queued, because an unbounded queue only converts CPU exhaustion into memory
  exhaustion.

Both are edited under **Settings** in the admin UI, or through
`GET`/`PUT /api/settings`, and take effect without a restart.

Measured against a running gateway, 200 bad credentials from one source:

| Configuration | Gateway CPU | Outcome |
| --- | --- | --- |
| Both limits off | 13.25 s | every request reached bcrypt |
| Defaults | 0.54 s | 8 reached bcrypt, 192 got 429 |
| Throttle off, cap only | 1.59 s | 24 reached bcrypt, 176 got 503 |

**Behind a load balancer**, every request arrives from the balancer's address, so
per-source throttling becomes all-or-nothing and should usually be disabled
(`auth_limit_enabled: false`). The gateway does not trust `X-Forwarded-For` for
identity. The hashing cap is unaffected and is what bounds CPU in that
deployment — the third row above is that case.

Credentials that pass bcrypt are cached briefly, which is what keeps legitimate
callers out of the hashing queue during an attack. The cache survives config
reloads: it is invalidated by construction — the key binds the stored password
hash — so a rotated password or a deleted user takes effect immediately without
the cache needing to be flushed.

Both limits export counters: `gateway_auth_rejected_total{reason="throttled"}`,
`gateway_auth_rejected_total{reason="saturated"}`, and
`gateway_auth_failures_total`.

### Fan-out reads

A fan-out instance pushes to every target in `push_urls`; `fan_out_mode` decides
only how the responses are aggregated (`any` succeeds if one target accepts,
`all` requires them all). The targets are therefore replicas, and reads try them
in the configured order until one answers.

- Transport failures and 5xx move on to the next target. A 4xx does not — that
  is the upstream answering, and a replica returns the same answer.
- **Each target has its own timeout.** Set `timeout_seconds` per target in the
  instance editor; leave it at 0 to use the gateway's `default_target_timeout`
  setting. Targets are independent systems — a local cluster and a remote DR
  site need not answer at the same speed — so the allowance belongs to the
  target rather than being divided out of one shared budget.
- **The whole read is bounded by the caller.** When the client disconnects the
  gateway abandons the attempt in flight and stops. There is no artificial
  overall deadline: the caller decides how long it is willing to wait.
- A target that fails repeatedly is skipped for a short cool-off, so one dead
  replica does not add a connection timeout to every read. It is tried again
  once the window elapses.

Target order is meaningful and is edited in the admin UI: target 1 is the one
normally queried, the rest are fallbacks. Reordering the list changes where reads
go.

This makes reads survive a target being **down**. It does not reconcile a target
that is **up but stale** — one that missed an earlier write. That is left to the
backends, which are replicated systems expected to reconcile themselves; the
gateway's job is to make the divergence visible rather than to repair it. Reads
are counted per target, so a replica that is failing shows up on the Overview
page and in `gateway_read_requests_total{instance,target,result}`, alongside
`gateway_read_failovers_total` and the existing
`gateway_partial_failures_total` for writes.

### API keys

A user can hold bearer API keys. A key is a credential, not a separate
permission model: authenticating with one is authenticating as its owner, and
the owner's grants decide everything that follows. To narrow what a key may do,
create a narrower user — which is how service accounts already work here.

Issue one under **Users → API keys** in the admin UI, or:

```bash
curl -u admin:PASSWORD -X POST https://gateway:9091/api/users/promtail-prod/apikeys \
  -H 'Content-Type: application/json' -d '{"label":"promtail-prod"}'
```

The response carries the only copy of the token; only its hash is stored, so it
cannot be retrieved again. Use it as a bearer credential:

```bash
curl -H "Authorization: Bearer obsgw_..." https://gateway:8080/loki/api/v1/push
```

Notes:

- **Rotation without downtime.** A user may hold several live keys, so a new one
  can be issued and deployed before the old is revoked. A password cannot do
  this — changing it is a hard cutover.
- **Revocation is immediate**, not at the next config reload, and deleting a
  user revokes its keys with it.
- **Expiry is optional** and off by default: an unattended shipper whose
  credential lapses on a forgotten date is an outage.
- **Last-used** is recorded (at most once a minute per key) so stale credentials
  can be identified and retired.
- **Data plane only.** The admin API still requires a password.
- Keys are hashed with SHA-256 rather than bcrypt. The secret is 256 random
  bits, so there is nothing to guess; a deliberately slow hash would protect
  nothing while putting every shipper request on the expensive path.

## Testing

Run tests: `go test ./...`
Run vet: `go vet ./...`

## Security Scanning

The build script automatically includes SBOM generation and vulnerability scanning after building the binary. SBOM generation uses Syft with version extracted from `obsgateway.yml`, and vulnerability scanning uses Grype on the generated SBOM.

## License

MIT