# Observability Gateway

The obsgateway is a Go-based observability data-layer gateway. It sits in front of Loki (logs), Mimir (metrics), and Tempo (traces) and exposes a single HTTP API that proxies/rewrites push and query traffic, injects tenant headers, and can fan writes out to multiple backends.

## Using

### MIMIR

Mimir needs to be configured to enable multitenancy, especially for queries. 

In the helm chart you can add:

```yaml
mimir:
  structuredConfig:
    multitenancy_enabled: true
    tenant_federation:
      enabled: true
``` 

or you can modify the mimir.yaml

```yaml
multitenancy_enabled: true
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

Every route requires HTTP Basic credentials for a gateway user holding a `write`
grant on the matching backend. A `read` grant does not authorize ingestion. The
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

## Testing

Run tests: `go test ./...`
Run vet: `go vet ./...`

## Security Scanning

The build script automatically includes SBOM generation and vulnerability scanning after building the binary. SBOM generation uses Syft with version extracted from `obsgateway.yml`, and vulnerability scanning uses Grype on the generated SBOM.

## License

MIT