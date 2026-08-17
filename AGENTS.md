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
