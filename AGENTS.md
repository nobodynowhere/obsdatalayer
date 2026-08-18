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
