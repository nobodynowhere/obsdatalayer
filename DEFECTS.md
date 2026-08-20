# Defects and Risks

Review scope: the gateway as it now stands — a bootstrap file carrying only the
database connection and the two listener addresses, all runtime configuration in
the database, Casbin RBAC over first-class tenants, bcrypt credentials, separated
data and admin listeners, and an embedded admin UI. Severity is assigned from the
perspective of production multi-tenant use.

Each entry below was re-verified against the current source. A summary of what
has been resolved since earlier revisions is at the end.

## P1 - Authorization is per backend type, not per instance

A grant names a backend kind (`loki`, `mimir`, `tempo`, or `*`), an action, and a
set of tenants. Instance selection remains path-only: `getInstance` checks that
the named instance exists and is of the right backend kind, and nothing more.

References:
- `internal/fanout/fanout.go:52`.
- Policy object vocabulary in `internal/auth/auth.go`.

Impact: any caller holding `loki:read` can query every configured Loki instance,
not only the ones intended for them. Their own tenant IDs are still injected, so
upstream scoping applies — but instances may be separate clusters, regions or
estates with different data and trust levels, and the gateway does not
distinguish them. Tenant isolation holds; instance isolation does not.

Fix direction: make the policy object the instance name (or an instance group),
with the backend kind derived from it, so a grant can be scoped to `loki-prod`
rather than to Loki in general.

## P1 - Failed authentication is an unauthenticated CPU denial-of-service

The bcrypt equalizer that closes the username-enumeration timing channel also
means a wrong password costs the same as a right one — roughly 76 ms of CPU.
There is no rate limiting, no per-source throttle and no account lockout.

References:
- `Service.Authenticate` compares against `dummyHash` for unknown users and
  against the stored hash for wrong passwords.
- Successful credentials are cached briefly, but failed credentials deliberately
  are not.

Impact: an unauthenticated client can saturate every core by sending garbage
credentials at modest request rates. The successful-credential cache does not
help, because failures never populate it. This is reachable on the data listener,
which binds all interfaces by default.

Fix direction: per-source rate limiting in front of the bcrypt path, plus a
failure counter with backoff.

## P1 - There is no tenant-issued token or API-key workflow

Creating a tenant creates a UUID that is used as the upstream `X-Scope-OrgID`
value. It does not issue a credential. Callers still authenticate as users with
HTTP Basic, and user or role grants decide which tenant UUIDs may be injected.

References:
- Tenant creation in `internal/adminapi/adminapi.go`, `createTenant`.
- Authentication in `middleware.BasicAuth`.

Impact: automation cannot be handed a narrow tenant-scoped token. Operators must
create a user, assign grants, and distribute that user's Basic credential. The
new successful-credential cache reduces bcrypt pressure for valid callers, but
it is still not an explicit telemetry-writer credential model.

Fix direction: add first-class API keys or tokens with tenant, backend and action
claims, revocation, rotation and last-used metadata.

## P1 - Fan-out has no delivery contract

`fan_out_mode: any` returns success when at least one target accepts the write.
There is no retry queue, replay mechanism, target health state, idempotency
strategy or operator workflow for the writes a failed target missed.

References:
- `doAnyMode` in `internal/fanout/fanout.go` reports partial failures in a
  response header and a counter, then returns success.

Impact: partial success is silent data loss unless the design explicitly accepts
eventual divergence between targets. For observability data that needs to be
stated and backed by operational mechanics — at minimum a durable backlog, or an
alert tied to `gateway_partial_failures_total`.

Debug logging now records which targets failed and which Mimir errors were
suppressed, so the divergence is at least observable after the fact.

## P1 - Fan-out instances have no correct read target

The config forbids setting both `url` and `push_urls`, and `GetQueryTarget`
returns the first push target when `push_urls` is present.

References:
- `internal/config/config.go`, `GetQueryTarget`.
- Query routes forward to that single target, for example in
  `internal/fanout/loki.go` and `internal/fanout/mimir.go`.

Impact: under `fan_out_mode: any` a write may land only on the second target
while every subsequent read queries the first, so data written successfully
appears to be missing. Even under `all` mode the first push target is not
necessarily the right query endpoint. The model needs an explicit read URL, or
query fan-out with result merging.

## P1 - Read paths are not label-scoped

Tenant scoping on reads is enforced: the gateway injects the caller's tenant
UUIDs as `X-Scope-OrgID` on every query. Label scoping is not. Label filtering
and injection are applied only on push, and queries are forwarded with the
caller's raw selectors.

References:
- Push rewrite in `handlePush` (`internal/fanout/fanout.go`).
- Query routes forward without touching selectors.

Impact: where labels form part of the tenancy model, writes are tagged but reads
are not constrained by those tags. Within a tenant a caller can query outside the
label scope their writes are confined to.

## P1 - Upstream credentials are stored as plaintext

Backend `basic_auth` values are ordinary columns in the instances and
push_targets tables. They are redacted in `GET /api/config`, in the instance API
and in the audit log, but they are plaintext at rest and readable by anyone with
database access. There is no design for secret references, environment expansion,
file references, rotation or audit of the secrets themselves.

References:
- `InstanceConfig.BasicAuth` and `PushTarget.BasicAuth` in
  `internal/config/config.go`; written verbatim by `saveInstance`.

Impact: a database backup or a read-only database credential yields every
upstream credential. Gateway user passwords are bcrypt hashed; upstream
credentials cannot be, because they must be replayed.

## P2 - A tenant can be deleted while instances still reference it

`deleteTenant` refuses to remove a tenant that any user or role grant references,
but it does not consult instance configuration. An instance `tenant_id` or push
target `tenant_id` pointing at the tenant is not checked.

References:
- `internal/adminapi/adminapi.go`, `deleteTenant`, inspects `ListUsers` and
  `ListRoles` only.
- Instance references are validated on load by `Config.ValidateTenants`.

Impact: deleting such a tenant succeeds, and the next config reload then fails
validation. The gateway keeps serving its last good config and logs a reload
error every interval, with no way back except recreating the tenant with the same
UUID. The reload failure is now clearly logged, but the trap remains.

## P2 - Fan-out buffers the whole body once per target

Push bodies are read fully into memory and then re-read per target. The total is
bounded — `max_body_bytes` defaults to 32 MiB and is enforced on Loki, Mimir and
Tempo — but a single request still holds the body resident while every target
streams its own copy from it.

References:
- `handlePush` reads the body with `io.ReadAll`.
- `doSingleTarget` creates a reader per target over the same buffer.

Impact: memory scales with concurrent pushes times body size. At the default cap,
a few hundred concurrent maximum-size pushes are enough to exhaust a modest
container.

## P2 - Admin access is all-or-nothing

The admin plane is gated by a single `admin:access` grant. There is no read-only
admin: anyone who can inspect the configuration can also reload it, create users,
assign roles, edit instances and delete tenants.

References:
- `middleware.AdminAuth` checks `CanAdmin` alone.
- Every route registered by `adminapi.Register` sits behind that one check.

Impact: an operator who needs only to read `/api/config` or scrape `/metrics`
must be granted full administrative control, including user management. The audit
log now records who did what, but does not constrain what they may do.

## P2 - Updating an instance changes its database identity

`UpdateInstance` deletes the existing row and its children and re-inserts them,
so the instance UUID changes on every edit.

References:
- `internal/config/db.go`, `UpdateInstance`.

Impact: nothing currently references an instance by UUID — the runtime keys on
name — so this is latent. It becomes a defect the moment anything external (an
audit record, a metric label, a foreign key) wants a stable instance identity.

## P2 - More than one gateway settings row is still possible

`loadSetting` now orders by primary key before taking the first row, so selection
is deterministic, and `EnsureSettings` creates the row only when the table is
empty. Nothing enforces that the table holds exactly one row.

References:
- `internal/config/db.go`, `loadSetting` and `EnsureSettings`.

Impact: a second row inserted by hand or by a future migration would be silently
ignored rather than reported. A singleton constraint, or an explicit "active"
marker, would make the intent enforceable.

## P2 - No compatibility or versioning strategy for the proxied APIs

The gateway exposes selected Loki, Mimir and Tempo routes under its own path
scheme, but does not define which upstream API versions are supported, how new
endpoints are added, or how unsupported endpoints fail. The admin API is likewise
unversioned.

References:
- Route subsets hard-coded in `internal/fanout/loki.go`, `mimir.go` and `tempo.go`.
- Admin routes are grouped under `/api`, but without a version prefix.

Impact: clients may assume the gateway is a transparent replacement for each
backend when it is a partial facade. Unversioned admin routes make future
breaking changes awkward, and the UI is now a first-class consumer of them.

## P2 - The unified backend abstraction is still superficial

Loki, Mimir and Tempo differ in protocol shape, query semantics, ingestion format
and tenancy model. They are unified here mostly by URL prefix and a shared
`InstanceConfig`, with backend-specific behaviour scattered across route handlers
and rewrite code. Tempo still has no equivalent of the label filter/inject policy
that Loki and Mimir share, and the instance editor has to special-case it.

Impact: adding backend behaviour will keep growing conditionals and inconsistent
feature support. A clearer design would define a capability contract per backend
covering push, query, tenant header handling, rewrite support and fan-out.

## P2 - Dark mode is incomplete

The admin UI exposes dark mode, but not every surface renders correctly under
the dark palette. Some controls, text, tables or nested panels have insufficient
contrast or retain light-mode assumptions.

Impact: operators using dark mode can miss important configuration state or find
parts of the UI hard to read. For an admin console that edits tenants, roles,
credentials and upstream routing, dark-mode readability is a usability and
operational-safety issue, not just cosmetic polish.

Fix direction: audit every admin view in dark mode, especially modal forms,
tables, empty states, select/dropdown menus and inline hints. Add visual
regression coverage or a screenshot checklist for both light and dark themes.

---

## FUTURE - Mimir tenant deletion, surfaced in the admin UI under Tenants

Loki's log deletion API is proxied and gated by the `delete` action. Mimir has no
equivalent: its reference states plainly that it does not support deleting
individual series or applying label matchers. What Mimir offers instead is
tenant-level destruction:

- `POST /compactor/delete_tenant` — deletes **all** data for the tenant named in
  `X-Scope-OrgID`.
- `GET /compactor/delete_tenant_status` — progress of the above.
- `POST /ruler/delete_tenant_config` — deletes every rule group for the tenant.
- `POST /multitenant_alertmanager/delete_tenant_config` — deletes the tenant's
  Alertmanager configuration.

None of these are requested by any Grafana data source, and none belong on a
tenant-facing data path: they are operator actions whose blast radius is an
entire tenant's history, and they are not recoverable.

Intended shape: plumb them into the admin UI under **Tenants**, as a delete
action on a tenant record, rather than exposing them as proxied routes. That puts
the capability behind an admin grant and an explicit confirmation on a named
tenant, instead of behind a data-plane grant a tenant could hold themselves.

Two things to settle when it is built:

- Whether deleting a tenant should also call the ruler and Alertmanager
  config-deletion endpoints, or only the compactor one.
- Whether `delete_tenant_config` on the ruler and Alertmanager should instead be
  reachable under `rules:write` and `alerts:write`, since that is what they
  destroy.

Until then, a `delete` grant is rejected on the `mimir` backend rather than
silently accepted and inert; see `controlActionBackends` in
`internal/auth/auth.go`.

## Deliberate design decisions

Recorded so they are not re-reported as defects.

- **The bootstrap file holds only database connection fields, listener addresses
  and listener TLS.** `db.type` selects SQLite or Postgres; SQLite uses `db.path`
  plus optional SQLite settings, while Postgres uses host, port, database, user
  and related connection fields. `gateway.listen`, `gateway.admin_listen` and
  `gateway.tls` are process-level values that cannot be read from the database.
  Everything else lives in the database and hot-reloads.
- **Listener addresses require a restart.** They are process-level concerns, so
  they are deliberately absent from the reloadable config.
- **The admin plane defaults to loopback.** A bare port binds `127.0.0.1`;
  binding wider requires `*:9091` or an explicit IP, and logs a warning.
- **A fresh install starts empty.** There is no seed mechanism; the gateway
  generates an admin password on first start and everything else is created
  through the UI or admin API. An empty instance list is a valid state.
- **`X-Scope-OrgID` carries the tenant UUID.** Grants and instances reference
  tenants by UUID only, so renaming a tenant never repoints where data lands.
- **Casbin subjects are prefixed by kind** (`u:` users, `r:` roles). Casbin keeps
  all subjects in one namespace, so without this a user and a role sharing a name
  would be the same subject.
- **A `*` data grant never confers admin.** The matcher excludes the admin object
  from the wildcard; admin must be granted explicitly as `admin:access`.
- **The UI bundle is served without credentials.** A browser cannot supply Basic
  auth for the initial document load, and the bundle carries no tenant data. The
  exemption is scoped to `ui.IsUIPath`; every API call it makes is authenticated.
- **`grafana_id` is unused.** Reserved for the future Grafana traffic proxy, and
  nullable so unassigned tenants do not collide on its unique index.
- **A redacted credential is resolved by URL, never by position.** An unresolvable
  mask is rejected rather than guessed at.

---

## Resolved since earlier revisions

- **SIGHUP terminated the process** while the packaged unit's `ExecReload` sent
  it — signals are now handled, SIGHUP reloads, and SIGTERM/SIGINT drain both
  listeners through `http.Server.Shutdown`.
- **An empty instance list was unrecoverable** — removing the last instance no
  longer wedges every subsequent reload.
- **Administrative mutations were not audited** — all sixteen mutating operations
  emit a started/finished pair naming the actor, target, status and duration,
  with the submitted body recorded at debug and credentials masked.
- **The active settings row was chosen arbitrarily** — selection is now ordered
  and deterministic (the remaining singleton gap is recorded above).
- **Repository hygiene** — a `.gitignore` now excludes build output, the UI
  bundle and the SQLite database, which holds bcrypt hashes and upstream
  credentials.
- **Nothing logged at debug level**, making the log-level control inert — there
  are now debug call sites across auth, middleware, proxy, fan-out, config and
  tenant covering authorization decisions, tenant injection, upstream calls,
  per-target fan-out results and error suppression.
- **A malformed bootstrap file could echo the database password** into the
  startup log — parse errors are routed through `redactYAMLError`.
- **Masked push-target credentials were matched positionally**, so reordering
  targets sent one upstream's password to another. They are now matched by URL.
- **Every successful data-plane request paid full bcrypt cost** — valid
  credentials are now cached briefly and the cache is invalidated on auth reload
  or password-hash change. Failed authentication remains a DoS risk and is
  recorded above.
- **Admin API routes lived at the root** — they now sit under `/api/...`, and the
  UI and dev proxy call the prefixed paths.
- **Configured instance or push-target tenants were not checked against grants**
  before use — the data path now verifies the authenticated caller is allowed for
  the effective target tenant and rejects ambiguous multi-tenant writes.
- **Admin lockout checks only understood the built-in admin role** — the admin
  API now simulates user and role mutations and refuses changes that would remove
  the last remaining admin access path.
- **Bootstrap could leave an existing user database with no usable admin** — the
  bootstrap pass now repairs the built-in admin role and creates or resets a
  recovery admin user when no current user has admin access.
- Earlier revisions also resolved: the global bearer token, unauthenticated
  `/config` and `/metrics`, the shared data/admin listener, `auth_passthrough`,
  backend URLs leaking in error responses, unvalidated upstream URLs, stale
  timeouts after reload, unbounded push bodies, and username enumeration by
  timing.
