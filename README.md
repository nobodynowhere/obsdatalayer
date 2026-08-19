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