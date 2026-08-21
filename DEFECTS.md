# Defects and Risks

Review scope: the gateway as it now stands — a bootstrap file carrying only the
database connection and the two listener addresses, all runtime configuration in
the database, Casbin RBAC over first-class tenants, bcrypt credentials, separated
data and admin listeners, and an embedded admin UI. Severity is assigned from the
perspective of production multi-tenant use.

**There are no open P1 entries.** Every P1 this register ever carried has been
closed and its entry removed. Two were closed as deliberate design decisions
rather than as fixes -- the fan-out delivery contract and per-instance
authorization -- and their reasoning is kept under those decisions below so
neither is re-reported as a defect. The entries that remain are real and open,
and the Tempo one is a genuine capability gap.

Entries were re-verified against the source on 2026-08-20 and again on
2026-08-21, with line references refreshed.

Tempo's read scoping needs a word, because it is what is left of the last P1.
Read label scoping was one entry covering Loki and Tempo. Loki is fixed. Tempo
is not, but the argument that made it a P1 -- writes tagged, reads unconstrained
-- does not apply there, because a Tempo instance cannot carry a label policy on
the write path either. What is left is a capability gap, recorded below at P2.

Three entries were added while verifying the above: the config reload route is
the only admin mutation that escapes the audit wrapper, encryption keys have no
rotation workflow, and the Loki read rewriter is a scanner rather than a full
LogQL parser.

Every entry here has been confirmed present in the source except **Dark mode is
incomplete**, which is carried forward on structural evidence and needs a visual
pass to confirm or close; the entry says so.

## P2 - Tempo has no label-scoped tenancy model

Tenant scoping on Tempo reads and writes is enforced by `X-Scope-OrgID`, but
Tempo still has no equivalent of the Loki and Mimir label filter/inject policy.
`read_label_selector` is intentionally rejected on Tempo grants rather than
accepted and ignored.

References:
- Tempo instances reject `labels` config in `internal/config/config.go`.
- `read_label_selector` is accepted only for Mimir and Loki read grants,
  `internal/auth/auth.go` and `internal/rewrite/promql.go`.
- Tempo read routes forward through `p.ForwardQuery` without a TraceQL or search
  attribute constraint, `internal/fanout/tempo.go`.

Impact: Tempo cannot participate in a label-scoped tenancy model. That is no
longer the same write/read mismatch that existed for Loki logs -- Loki reads are
now constrained where Loki writes are tagged -- but traces cannot yet be scoped
by attributes at the gateway.

Fix direction: define a Tempo policy model first: which attributes can be
filtered or injected on each supported write format, how the same policy maps
onto TraceQL and search endpoints, and which Tempo endpoints must be refused
when the policy cannot be expressed.

## P3 - Loki read-policy rewriting is not backed by a full LogQL parser

Loki read policies are now enforced fail-closed for supported query endpoints,
but the gateway does the rewrite with a conservative stream-selector scanner
rather than a full LogQL AST. The scanner recognizes brace-delimited stream
selectors outside quoted strings and validates each candidate with the existing
Prometheus selector parser before merging the policy selector.

References:
- `ConstrainLogQL`, `internal/rewrite/logql.go`.
- The Loki read hook before forwarding, `internal/fanout/loki.go`.
- Existing PromQL read policies use the Prometheus parser directly,
  `internal/rewrite/promql.go`.

Impact: the current behavior is safer than accepting a restricted Loki read and
forwarding it unconstrained: requests that cannot be constrained are rejected.
The tradeoff is compatibility risk. Future LogQL syntax, or valid edge cases
that do not look like ordinary stream selectors to the scanner, may be rejected
until the rewriter learns them. The larger concern is maintainability: policy
rewrites should ideally be AST-based so they track LogQL semantics instead of a
gateway-local approximation.

Fix direction: adopt Loki's LogQL parser when the dependency cost is acceptable,
rewrite stream selector nodes in the parsed AST, and keep the existing fail-closed
behavior for endpoints or query forms the parser cannot represent safely.

## P2 - Encrypted credentials have no rotation workflow

Upstream credentials are encrypted at rest, but changing the encryption key is a
manual exercise. There is no command that re-encrypts every stored credential
under a new key, and no way to run with an old key accepted for reads while a
new one is used for writes.

The envelope carries a version (`enc:v1:`), so a second scheme can be introduced
without ambiguity, but nothing consumes that version yet.

References:
- Envelope format and the version prefix, `internal/secret/secret.go`.
- The startup reconciliation that would host a rotation pass,
  `config.EnsureCredentialsEncrypted` in `internal/config/encryption.go`.

Impact: rotating the key today means decrypting with the old key and
re-encrypting with the new one by hand, or re-entering every upstream credential
through the admin UI. An operator who suspects the key is compromised has no
supported path that keeps the gateway serving.

Fix direction: a `--rotate-encryption-key` pass that reads with the old key and
writes with the new one in a single transaction, plus an optional secondary
decryption key so the rotation can be staged across a restart.

## P2 - A tenant can be deleted while instances still reference it

`deleteTenant` refuses to remove a tenant that any user or role grant references,
but it does not consult instance configuration. An instance `tenant_id` or push
target `tenant_id` pointing at the tenant is not checked.

References:
- `deleteTenant`, `internal/adminapi/adminapi.go:449`, inspects `ListUsers` and
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
- `handlePush` reads the body with `io.ReadAll`,
  `internal/fanout/fanout.go:328`.
- `doSingleTarget` creates a `bytes.NewReader` per target over the same buffer,
  `internal/fanout/fanout.go:458`.

Impact: memory scales with concurrent pushes times body size. At the default cap,
a few hundred concurrent maximum-size pushes are enough to exhaust a modest
container.

## P2 - Config reload is the one unaudited mutation

Every mutating admin operation is wrapped in `h.audited(...)`, which emits a
started/finished pair naming the actor. `POST /api/config/reload` is registered
directly on the admin mux in `main.go`, outside `adminapi.Register`, and carries
no such wrapper.

References:
- The fifteen audited mutations, `internal/adminapi/adminapi.go:43`-`:69`.
- The unaudited reload handler, `main.go:466`.

Impact: a reload swaps the entire runtime config and auth snapshot — it is the
operation that makes every other pending edit take effect — and it leaves no
record of who triggered it. An operator reconstructing a change window from the
audit log sees the edits but not the moment they went live.

Fix direction: move the route into `adminapi.Register` so it inherits `audited`,
or wrap the handler in `main.go` with the same helper.

## P2 - Admin access is all-or-nothing

The admin plane is gated by a single `admin:access` grant. There is no read-only
admin: anyone who can inspect the configuration can also reload it, create users,
assign roles, edit instances and delete tenants.

References:
- `middleware.AdminAuth` checks `CanAdmin` alone,
  `internal/middleware/auth.go:97`.
- All 28 routes registered by `adminapi.Register`
  (`internal/adminapi/adminapi.go:38`-`:69`) sit behind that one check, as do
  `GET /api/config` and `POST /api/config/reload` in `main.go:454` and `:466`.

Impact: an operator who needs only to read `/api/config` or scrape `/metrics`
must be granted full administrative control, including user management. The audit
log now records who did what, but does not constrain what they may do.

## P2 - Updating an instance changes its database identity

`UpdateInstance` deletes the existing row and its children and re-inserts them,
so the instance UUID changes on every edit.

References:
- `UpdateInstance`, `internal/config/db.go:137`, deletes the row and its children
  then calls `saveInstance`, which mints a fresh UUID.

Impact: nothing currently references an instance by UUID — the runtime keys on
name — so this is latent. It becomes a defect the moment anything external (an
audit record, a metric label, a foreign key) wants a stable instance identity.

## P2 - More than one gateway settings row is still possible

`loadSetting` now orders by primary key before taking the first row, so selection
is deterministic, and `EnsureSettings` creates the row only when the table is
empty. Nothing enforces that the table holds exactly one row.

References:
- `EnsureSettings`, `internal/config/db.go:46`, and `loadSetting`,
  `internal/config/db.go:102`.
- `dbstore.GatewaySetting` (`internal/db/models.go`) carries a primary key and no
  uniqueness or singleton constraint.

Impact: a second row inserted by hand or by a future migration would be silently
ignored rather than reported. A singleton constraint, or an explicit "active"
marker, would make the intent enforceable.

## P2 - No compatibility or versioning strategy for the proxied APIs

The gateway exposes selected Loki, Mimir and Tempo routes under its own path
scheme, but does not define which upstream API versions are supported, how new
endpoints are added, or how unsupported endpoints fail. The admin API is likewise
unversioned.

References:
- Route subsets hard-coded in `internal/fanout/loki.go`, `mimir.go` and
  `tempo.go`. Each documents its own gaps in a `Not implemented` doc comment —
  Loki omits the deprecated `/api/prom` query aliases, Mimir omits tenant limits
  and stats and block upload, Tempo reports full query coverage — but the gaps
  are documented per file, not stated as an API contract.
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

This entry is carried forward on structural evidence rather than a fresh visual
pass — it is the one item in this register that reading the source cannot settle
either way. The structure is what keeps it open: `ui/src/assets/app.css` reassigns
semantic tokens for `.my-app-dark` and then overrides the DDS chrome by selector,
because, in its own words, DDS hardcodes light colours on bare elements and "a
long tail of generated class names that cannot practically be enumerated." A
theme built by enumerating overrides is incomplete until proven otherwise, and
nothing enumerates it. The application's own views are clean — a grep for
hardcoded light colours across `ui/src/views` and `ui/src/components` finds only
a `white-space` property — so any remaining gaps live in that override layer.

References:
- Token reassignment and the DDS override layer, `ui/src/assets/app.css:13` and
  `:44` onward (342 lines, 27 dark rules).
- Nine views to audit, `ui/src/views`.

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
silently accepted and inert; see `controlActionBackends`,
`internal/auth/auth.go:256`.

One rough edge in that rejection: `controlActionGaps` (`internal/auth/auth.go:277`)
supplies the explanatory clause appended to the error, and it has an entry for
`loki` but none for `mimir`. A rejected `mimir` `delete` grant therefore fails
with a truncated message ending in a bare colon:

    action "delete" is not supported on the mimir backend:

Adding a `mimir` entry saying that Mimir supports no selector-based deletion, only
tenant-level destruction, would make the error say what the comment above the map
promises it says.

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
- **Reconciling divergent fan-out targets is the upstream's job.** A fan-out
  instance pushes every write to every target; `fan_out_mode` decides only how
  the responses are aggregated, so `any` reports success when at least one
  target accepted. That can leave targets holding different data when a push
  fails against one of them, and the gateway does not attempt to repair it — no
  retry queue, no replay log, no anti-entropy. Loki, Mimir and Tempo are
  replicated systems in their own right and are expected to reconcile, so
  duplicating that here would mean a second, worse implementation of something
  the backends already do.

  What the gateway owes instead is visibility, and it is expected to keep it: a
  partial write is reported in the `X-Gateway-Partial-Failure` response header
  and counted by `gateway_partial_failures_total`, the failing target and any
  suppressed Mimir error are named at debug level, and reads count success and
  failure per target (`gateway_read_requests_total`) so a replica that is
  falling behind is visible on the Overview page rather than only in a
  divergence nobody notices.
- **An API key is a credential, never a scope.** A key belongs to one user and
  inherits that user's grants unchanged. Narrowing what a key may do means
  creating a narrower user, which is how service accounts already work here.
  Giving keys their own grants would duplicate the authorization model onto a
  second object and raise the question of what happens when the owner's grants
  shrink below the key's.
- **API keys are a data-plane credential.** They are long-lived and issued for
  unattended shippers; the admin plane creates users and edits routing, so it
  keeps requiring a password. Same instinct as the wildcard grant that excludes
  the admin object.
- **Grants scope callers to backends and tenants; the instance is chosen by the
  gateway, never named by the caller.** Two different things get called
  "instance scoping", and only one of them is absent. Stating both is the point
  of this entry, because the earlier wording claimed instances of a kind were
  interchangeable, and they are not.

  Instances are *not* interchangeable to the gateway. Each carries its own
  upstream credential, tenant ID and TLS setting, and each push target within an
  instance may override all three — `resolveTarget` in
  `internal/config/config.go`. Every request is authenticated to the specific
  backend it lands on, including after a read failover moves it to another
  target. That per-target identity is a decision in its own right and is
  recorded immediately below.

  What callers cannot do is choose one. No data-plane route contains an instance
  name: the public paths are the backends' own API paths, and `selectInstance`
  in `internal/fanout/fanout.go` resolves the instance from the caller's
  tenants. An instance whose targets carry tenant IDs is eligible only to a
  caller holding every one of them; an instance whose targets carry none is
  shared and eligible to any caller with the backend grant; a tenant-bound match
  wins over a shared one; and when more than one instance is eligible the
  request is refused with 409 rather than settled by guessing. At most one
  instance is selectable for a given caller, backend and direction.

  So an instance-scoped grant would have nothing to attach to. The caller cannot
  express a preference, and cannot reach a tenant-bound instance whose tenants
  they do not hold. What remains is that two callers with different tenants can
  share one tenant-less instance and be separated by `X-Scope-OrgID` alone —
  which is the isolation boundary this gateway is built on, applied per request
  rather than per backend.

  This was previously recorded as a defect proposing that the policy object
  become the instance name. That is rejected on the grounds above, and because
  it would tie authorization policy to backend topology: moving or renaming an
  instance would silently rewrite who can read what.

- **How the gateway authenticates to a backend is unrelated to grants.** This is
  the other half of the decision above: instances are distinct to the gateway
  even though callers cannot select between them. Each instance carries its own
  upstream credential, and each push target may override it. Two clusters behind
  one instance can require entirely different credentials. This is deliberately
  independent of the caller's grants: the caller proves who they are to the
  gateway, and the gateway separately proves who it is to each backend.

  The pairing is per target throughout, and covered by tests on every path that
  carries it: fan-out writes present each target its own credential and tenant,
  reads present the credential of whichever target is being tried including
  after a failover, a target with no credential of its own inherits the
  instance's, and reordering targets in the admin UI moves each credential with
  its own URL rather than its position.

- **Read preference follows the configured push-target order.** The first target
  is the one queried; the rest are fallbacks. It is stored as an explicit
  position rather than left to the database's row order, so reordering the list
  in the admin UI changes where reads go.

---

## Resolved since earlier revisions

- **Push-target order was not preserved by the database** — found while making
  the read order configurable. `push_targets` had no position column and was
  loaded without an ORDER BY, so the order rows came back in was the database's
  choice; the primary key is a random UUID, so ordering by it is arbitrary.
  Targets now carry an explicit position, and the admin UI has controls to
  reorder them. Verified by re-running the ordering tests against an arbitrary
  row order, which returns targets scrambled and silently ignores a reorder.
- **The encryption envelope prefix could bypass encryption on write** —
  `Cipher.Encrypt` short-circuited on `IsEncrypted` to make itself idempotent,
  so a credential whose text began with `enc:v1:` was written to the database
  verbatim, in plaintext, with a key configured. The shortcut is gone: every
  non-empty value is sealed. The prefix describes what the gateway itself wrote
  and was never evidence about a value arriving from outside. Nothing needed the
  idempotence — the startup migration partitions on the prefix and only ever
  hands `Encrypt` plaintext — and a test now pins that it still does not
  double-wrap. Reproduced before the fix and covered afterwards at both the
  cipher and the storage layer, for instance and push-target credentials.
- **The credential cache was wiped on every config reload** — `Service.Reload`
  no longer clears it. Invalidation was already structural and is now pinned by
  tests: the cache key binds the stored password hash and is always checked
  against the freshly loaded one, so a rotated password cannot match a stale
  entry; a deleted user fails the snapshot lookup before the cache is consulted;
  and the cache covers authentication only, so a revoked grant still takes
  effect at once. Clearing was also the only thing bounding the map, since
  lookups evict just the key they touch, so the cache gained its own TTL sweep.
  Measured over 24 requests spanning several reloads: tail latency fell from
  91 ms to 6.3 ms and gateway CPU from 0.59 s to 0.02 s, with revocation
  verified still immediate against a running gateway.
- **Instance selection ignored tenancy** — `selectInstance` now prefers a
  tenant-bound instance the caller holds, falls back to shared instances, and
  returns 409 instead of guessing when the choice is ambiguous. Scoping grants
  to instances rather than to backends and tenants was considered and rejected;
  the reasoning is under the deliberate design decisions above.
- **SIGHUP terminated the process** while the packaged unit's `ExecReload` sent
  it — signals are now handled, SIGHUP reloads, and SIGTERM/SIGINT drain both
  listeners through `http.Server.Shutdown`.
- **An empty instance list was unrecoverable** — removing the last instance no
  longer wedges every subsequent reload.
- **Administrative mutations were not audited** — the fifteen mutating operations
  registered by `adminapi.Register` emit a started/finished pair naming the
  actor, target, status and duration, with the submitted body recorded at debug
  and credentials masked. `POST /api/config/reload` is the one mutation still
  outside that wrapper; it is recorded above.
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
  or password-hash change. The separate denial-of-service risk in failed
  authentication has since been closed by the per-source throttle and the
  process-wide hashing gate in `internal/authlimit`.
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
