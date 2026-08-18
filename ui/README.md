# Administration UI

Vue 3 + PrimeVue 4 single-page app for managing the gateway's tenants, users,
roles and configuration. It is compiled into the gateway binary, so there is no
separate web server to deploy.

## How it is wired

| Concern | Where |
|---|---|
| Build output | `../internal/ui/dist` (set by `vite.config.js`) |
| Embedded by | `internal/ui/ui.go` via `//go:embed all:dist` |
| Served at | `/ui/` on the **admin** listener (loopback:9091 by default) |
| Router base | `/ui/` — must stay in sync with `ui.Prefix` in Go |

The gateway falls back to `index.html` for unknown paths under `/ui/`, so client
routes survive a refresh or a deep link.

## Authentication

The admin API uses HTTP Basic on every request. The SPA collects credentials on
the sign-in screen, validates them against `GET /api/whoami`, and holds the encoded
credential in `sessionStorage` — it is cleared when the tab closes and never
written to `localStorage` or a cookie. An axios interceptor attaches it to every
request and treats a 401 as a lost session.

**The static bundle itself is served without credentials.** A browser cannot
supply Basic auth for the initial document load, and the bundle contains no
tenant data — only markup, JS and CSS. Everything it *displays* comes from the
authenticated endpoints. This exemption is implemented in `middleware.AdminAuth`
and is scoped to `ui.IsUIPath`.

## Dell Design System

`@dds/components` comes from Dell's internal Artifactory; `.npmrc` sets the
scoped registry. DDS normally loads its fonts and icon font from `dds.dell.com`,
so those two SCSS entrypoints are deliberately **not** imported — local copies
are vendored in `public/` and linked from `index.html` instead, which keeps the
UI working on an air-gapped network.

PrimeVue is themed with a Dell palette in `src/theme/preset.js`, and its
generated CSS is placed in a cascade layer ordered after DDS so DDS layout wins.

## First build on a machine with registry access

No `package-lock.json` is committed yet: `@dds/components` resolves only from
Dell's internal Artifactory, so a correct lock can only be produced somewhere
that can reach it. On such a machine:

```bash
cd ui && npm install     # resolves @dds and writes package-lock.json
npm run build
```

Commit the generated `package-lock.json`. After that, `npm ci` works everywhere
with registry access and the build is reproducible.

## Development

```bash
npm install     # or npm ci once a lock file is committed
npm run dev
```

`npm run dev` serves on :5173 and proxies the admin API endpoints to
`127.0.0.1:9091`, so run the gateway alongside it.

## Production build

```bash
npm ci && npm run build   # writes ../internal/ui/dist
cd .. && go build -o obsgateway .
```

From the repository root, `build.sh` builds the UI as part of a normal build:

```bash
./build.sh                                    # UI + binary + RPM + container
./build.sh -skiprpm -skipcontainer            # UI + binary only
./build.sh -skipui -skiprpm -skipcontainer    # binary only, reuse existing bundle
```

On Windows, use the PowerShell build script from the repository root:

```powershell
.\build.ps1
.\build.ps1 -SkipRpm -SkipContainer
.\build.ps1 -SkipUi -SkipRpm -SkipContainer
```

`build.bat` is a `cmd.exe` wrapper around the same PowerShell script.

`-skipui` follows the same convention as the script's existing `-skiprpm` and
`-skipcontainer` flags: everything is built by default, and each stage has an
opt-out.

Only `internal/ui/dist/.gitkeep` is tracked — the bundle itself is a build
artifact. The directory has to exist for `//go:embed` to compile, so a fresh
clone builds fine without Node; `/ui/` simply reports "ui not built" until the
bundle is produced.
