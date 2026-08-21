# Project Notes

## Build and Verification

- Run tests: `go test ./...`
- Run vet: `go vet ./...`
- CGO-free build (matches `build.sh`):

  PowerShell:
  ```powershell
  $env:CGO_ENABLED=0
  go build -a -trimpath -o build/obsgateway-linux-amd64 .
  ```

  Bash:
  ```bash
  env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -trimpath -o build/obsgateway-linux-amd64 .
  ```

## Configuration

- The runtime source of truth is the configured database (SQLite or PostgreSQL).
- The file passed to `-config` is a minimal *bootstrap* file that opens the DB,
  sets listener ports, and may contain an optional `seed` block to populate a
  fresh database on first startup.
- The gateway and admin ports in the bootstrap file override any values stored
  in the database.
- The config and auth snapshots are reloaded automatically on a ticker and can
  also be triggered via `POST /config/reload` on the admin port.

## Snyk

- `snyk` CLI is installed at `C:\WINDOWS\system32\snyk.exe`, but running
  `snyk code test` requires an authenticated Snyk account (`snyk auth`).
  When the tool is unavailable/unauthenticated, rely on `go vet`, `go test`,
  and manual security review.

## Credential Encryption

- Upstream backend credentials (`basic_auth` on instances and push targets) are
  encrypted at rest with AES-256-GCM (`internal/secret`).
- The key comes from `OBSGATEWAY_ENCRYPTION_KEY` or the file named by
  `gateway.encryption_key_file` in the bootstrap config, in that order. Key
  files must be mode 0600.
- Generate a key: `obsgateway --generate-encryption-key --encryption-key-file PATH`
  (omit the path to print it to stdout).
- Encryption happens at the database boundary only: `saveInstance` and
  `savePushTarget` encrypt, `mapInstance` decrypts. Everything above that layer
  — the proxy, API redaction, by-URL mask resolution — sees plaintext and is
  unaffected.
- `config.EnsureCredentialsEncrypted` runs at startup. It migrates pre-encryption
  plaintext in place and refuses to start when stored credentials cannot be
  protected or read with the configured key.
- A nil `*secret.Cipher` means "no key configured": it passes plaintext through
  and rejects ciphertext. Tests pass `nil` where encryption is not under test.

## Authentication Throttling

- `internal/authlimit` holds both defences: `Limiter` (per-source failure
  backoff) and `Gate` (process-wide cap on concurrent bcrypt).
- The gate is consulted inside `auth.Service.AuthenticateContext`, **after** the
  credential cache. That ordering is load-bearing: a cached valid credential
  must never queue behind an attacker's hashing.
- `middleware.AuthGuard` bundles the limiter with its metrics. A nil guard
  disables throttling, which is what tests pass.
- Each listener gets its own `AuthGuard` (`newAuthGuards` in `main.go`). Do not
  merge them: a shared counter lets a data-plane flood lock the operator out of
  the admin API.
- The source key is the transport peer address; `X-Forwarded-For` is not
  trusted. Behind a proxy, per-source throttling should be disabled and the gate
  relied on instead.
- The credential cache is **not** cleared by `Service.Reload`, and must not be.
  Invalidation is structural: the key binds the stored password hash, the user
  snapshot is checked before the cache, and authorization is resolved per
  request. The cache sweeps itself on a TTL; do not reintroduce a blanket clear,
  which put every valid caller back through bcrypt on every reload.
- Settings live in the gateway settings row and hot-reload via `applyAuthLimit`.
  `auth_limit_enabled` is a nullable bool so a pre-upgrade row reads as NULL and
  defaults to on rather than off.

## Read Failover

- `Proxy.ForwardQuery` tries an instance's targets in configured order
  (`InstanceConfig.GetReadTargets`), not just the first. Fan-out always pushes
  to every target — `fan_out_mode` only aggregates responses — so targets are
  replicas and any can serve a query.
- Only transport errors and 5xx fail over. A 4xx is the upstream answering.
- Each attempt is bounded by its target's `timeout_seconds`, falling back to the
  `default_target_timeout` setting (`PushTarget.Timeout`,
  `Proxy.SetDefaultTargetTimeout`). The whole read is bounded by the request
  context, i.e. caller disconnect. Do not reintroduce a shared budget divided
  between targets: it made each target's allowance depend on the replica count.
- Reads use `Proxy.ReadClient()`, which shares the query client's transport but
  carries no `http.Client.Timeout`. That is deliberate — a client-level timeout
  would silently cap a target configured to allow longer. Tests that need a
  short read bound must call `SetDefaultTargetTimeout`, not set a client timeout.
- Nothing is written to the client until an attempt is known good: see
  `readAttempt.commit` / `.discard`. Do not write headers before that decision.
- `targetHealth` (`internal/proxy/health.go`) skips a repeatedly-failing target
  for a short cool-off. It is keyed by URL and owned by the Proxy so it survives
  config reloads; keyed by instance it would reset every reload interval and
  never reach the threshold. It never returns an empty target list.
- POST reads (form-encoded queries) are buffered up to `maxReplayableReadBody`
  so they can be replayed; a larger body is streamed to a single target.
- Push-target order is load-bearing (target 1 is preferred for reads) and is
  persisted as `push_targets.position`. The load must keep its `Order("position")`
  — without it the row order is the database's choice and reordering in the UI
  silently does nothing.
- Reads are counted per target via `Proxy.recordRead` →
  `gateway_read_requests_total{instance,target,result}`, with
  `gateway_read_failovers_total` for reads that needed more than one target. The
  metrics sink is optional (`Proxy.SetMetrics`); all uses are nil-guarded. New
  instance-labelled counter sets must be added to `RetainInstances` or they leak
  series for deleted instances.

## API Keys

- A key is a credential for a user and inherits that user's grants. Do not give
  keys their own grants: that duplicates the authorization model onto a second
  object (`internal/auth/apikey.go`).
- Only the SHA-256 hash is stored. bcrypt is deliberately not used — the secret
  is 256 random bits, and bcrypt would put every shipper request on the hash
  path the auth throttle exists to protect.
- The token is `obsgw_<handle>_<secret>`. The secret is base64url, whose
  alphabet contains `_`, so `parseAPIKey` must use `SplitN(..., 3)`. An
  unlimited split rejected roughly half of all issued keys.
- Keys authenticate on the data plane only; `middleware.AdminAuth` does not
  accept bearer tokens.
- The authentication index is in memory and refreshed by `Service.Reload`, so
  issuing and revoking take effect immediately. `last_used_at` writes are
  throttled to one per key per minute.

## Dependencies

- Uses the pure-Go SQLite driver `github.com/glebarez/sqlite` so the binary can
  be built with `CGO_ENABLED=0`.

## Security Scanning

- The build script (`build.sh`) automatically includes SBOM generation and vulnerability scanning after building the binary
- SBOM generation uses Syft with version extracted from `obsgateway.yml` (currently v26.8.1)
- Vulnerability scanning uses Grype on the generated SBOM
- For manual scanning, you can run:
  ```bash
  # Extract version from obsgateway.yml
  package_version=$(grep 'version:' obsgateway.yml | awk '{print $2}' | tr -d '"' | sed 's/^v//')
  syft scan dir:. --source-name "obsgateway" --source-version "$package_version" --exclude ./.git --exclude ./ui/node_modules -o syft-json=sbom/syft.json -o spdx-json=sbom/spdx.json -o syft-text=sbom/sbom.txt -o syft-table=sbom/table.txt
  grype sbom/syft.json
  ```
- This generates SBOM files in the `sbom/` directory and checks for known vulnerabilities
- Run these commands after every build unless explicitly told otherwise
