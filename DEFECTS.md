# Defects and Risks

Review scope: current working tree changes aimed at unifying Loki, Mimir, and Tempo connections and adding tenantization. Severity is assigned from the perspective of production multi-tenant use.

## P0 - Empty gateway tokens are accepted

`config.Load` does not validate that `gateway.token` is present or non-empty. If the token is omitted, `middleware.BearerAuth` is initialized with `""`, and a request with `Authorization: Bearer ` passes because the trimmed token also equals `""`.

References:
- `internal/config/config.go:127-219` validates many fields, but not `cfg.Gateway.Token`.
- `internal/middleware/auth.go:18-19` compares the bearer value directly to the configured token.
- `test.md:43-46` says missing token should reject startup, but the implementation does not enforce that.

Impact: the gateway can be started in a weak authentication mode accidentally. In a tenantized gateway, this is a critical boundary failure.

## P0 - `/config` exposes every secret and tenant mapping

`GET /config` marshals and returns the active config object directly. That includes the gateway bearer token, backend `basic_auth` values, tenant IDs, backend URLs, and all configured tenant instances.

References:
- `cmd/gateway/main.go:49-58`
- Secret-bearing fields are defined in `internal/config/config.go:41-67`.

Impact: any holder of the single gateway token can retrieve credentials and tenant routing for every configured backend. This collapses tenant isolation and creates a credential exfiltration endpoint.

## P0 - Tenant authorization is only path selection plus upstream header injection

The gateway has one global bearer token and no mapping from caller identity to allowed instances or tenant IDs. Once authenticated, a client can call any configured path like `/api/{instance}/...`; `getInstance` only verifies that the path instance exists and matches the backend type.

References:
- Global auth token is installed in `cmd/gateway/main.go:82`.
- Instance lookup is path-only in `internal/fanout/fanout.go:51-58`.
- Tenant IDs are static upstream header values in `internal/config/config.go:58-59` and `internal/proxy/proxy.go:102-103`.

Impact: this is tenantization of upstream requests, not tenant authorization. Any authenticated caller can access every tenant instance configured in the gateway.

## P1 - `auth_passthrough` does not pass through `Authorization`

Despite the name and the lab test plan, passthrough mode explicitly strips the inbound `Authorization` header. It forwards `X-Scope-OrgID`, but not the caller's upstream auth identity.

References:
- `internal/proxy/proxy.go:79-83`
- `test.md:398-408` expects `Authorization: Bearer lab-token` to be present upstream.

Impact: passthrough mode cannot support upstream auth passthrough as documented. If the design is to forward end-user auth, the gateway needs a separate gateway-auth mechanism so the same `Authorization` header is not consumed by the gateway and lost before the backend.

## P1 - Config reload leaves gateway auth, port, and timeouts stale

`POST /config/reload` swaps the config used by route handlers, but the server port, middleware token, and HTTP client timeouts were all created from the original config and are never updated.

References:
- Initial clients are created in `cmd/gateway/main.go:35-36`.
- Middleware captures the original token in `cmd/gateway/main.go:82`.
- Reload swaps only `ConfigHolder.cfg` in `internal/config/holder.go:30-38`.

Impact: after reload, `GET /config` can show a new token while the gateway still authenticates with the old token. Timeout changes silently do nothing until restart. This is easy to misoperate and dangerous during credential rotation.

## P1 - Fan-out write instances have no correct read target model

The config forbids setting both `url` and `push_urls`, and `GetQueryTarget` chooses the first push target when `push_urls` is present.

References:
- `internal/config/config.go:163-166` rejects `url` plus `push_urls`.
- `internal/config/config.go:247-252` returns `push_urls[0]` for queries.
- Query routes forward directly, for example `internal/fanout/loki.go:25-48` and `internal/fanout/mimir.go:24-52`.

Impact: for `fan_out_mode: any`, a successful write may land only on the second target, while subsequent reads always query the first target. Even in `all` mode, the first push target may not be the correct query endpoint. The model needs an explicit read URL or a query fan-out/aggregation strategy.

## P1 - Read paths do not enforce label or tenant scoping

Label filtering and injection are applied only on push. Query requests are forwarded with the caller's raw query parameters. There is no read-side enforcement that Loki or Mimir queries are constrained to the injected labels or tenant policy.

References:
- Push rewrite happens in `internal/fanout/fanout.go:131-139`.
- Query forwarding does not rewrite or constrain selectors in `internal/fanout/loki.go:25-48` and `internal/fanout/mimir.go:24-52`.

Impact: if labels are being used as part of tenantization, writes may be tagged but reads are not constrained by those tags. A caller with access to an instance can ask the backend for data outside the intended label scope if the upstream tenant permits it.

## P1 - Push handlers read entire telemetry payloads into memory

All push bodies are read fully into memory, then duplicated across fan-out goroutines. There is no request size limit.

References:
- `internal/fanout/fanout.go:125-140`
- `internal/fanout/fanout.go:73-82`

Impact: large remote-write, Loki, or trace payloads can cause high memory use or denial of service. This is amplified by fan-out because one body is reused for every target.

## P1 - Unauthenticated metrics leak internal topology

`/metrics` bypasses auth, and fan-out metrics include the full target URL as a label.

References:
- `/metrics` skips auth in `internal/middleware/auth.go:13-15`.
- Metrics are labeled by `target` in `internal/metrics/metrics.go:15-22`.
- The full upstream URL is recorded in `internal/fanout/fanout.go:91`.

Impact: unauthenticated callers can discover instance names, internal backend URLs, status codes, and failure patterns. In a multi-tenant deployment, this exposes topology and operational state across tenants.

## P2 - Error responses and partial failure headers leak backend URLs

Partial fan-out failures expose target URLs in `X-Gateway-Partial-Failure`; all-mode failures include the target URL in the JSON body.

References:
- `internal/fanout/fanout.go:40-47`
- `internal/fanout/fanout.go:141-143`
- `internal/fanout/fanout.go:190-193`

Impact: clients can learn internal backend hostnames and fan-out topology. This is especially undesirable if one gateway token is shared across multiple tenant-facing clients.

## P2 - Upstream URLs are not validated at config load

The config validator only checks that URL strings are non-empty. It does not parse URLs or require `http`/`https` scheme and host.

References:
- `internal/config/config.go:168-170`
- `internal/config/config.go:202-207`

Impact: invalid URLs are accepted at startup or reload and fail later at request time. This weakens config reload safety and makes tenant changes harder to validate before activation.

## P2 - Repository and build hygiene issues

The working tree contains untracked generated artifacts and support files: `gateway` is a built Mach-O binary, plus `build.sh` and `test.md`. The module directive was also raised from `go 1.22.0` to `go 1.26.1`.

References:
- `go.mod:3`
- `gateway` is an untracked local binary.
- `build.sh` and `test.md` are untracked.

Impact: committing local binaries or environment-specific build scripts pollutes the repository. Raising the Go directive forces every builder onto Go 1.26.1 or toolchain auto-download, which may not be intended for this service.

## Design-Level Defects

These are broader design issues observed by comparing the lab/design sketch in `test.md` with the implementation and the intended goal of unifying Loki, Mimir, and Tempo with tenantization.

### P0 - Data-plane and admin-plane access are not separated

The same bearer token used by telemetry clients also authorizes config inspection and reload operations. Even if `GET /config` were redacted, `POST /config/reload` is still an administrative operation exposed on the same listener and protected by the same credential as push/query traffic.

References:
- Data routes and config routes are registered on the same mux in `cmd/gateway/main.go:40-80`.
- One middleware token protects all non-health/metrics routes in `cmd/gateway/main.go:82`.

Impact: every data-plane client is effectively an administrator. A tenantized gateway should have separate admin authentication, separate admin listen address, or both.

### P0 - The tenant model is not first-class

The design represents tenants indirectly through instance names and upstream `tenant_id` values. There is no tenant object, no user/principal object, no policy mapping principals to instances, no tenant lifecycle, and no way to express that one client may write but not query a tenant.

References:
- `InstanceConfig` has `Name` and `TenantID`, but no principal or ACL fields in `internal/config/config.go:52-61`.
- The only authorization decision is instance lookup in `internal/fanout/fanout.go:51-58`.

Impact: this cannot enforce tenant isolation beyond whatever each upstream backend happens to enforce. The gateway itself has no tenant boundary.

### P1 - Gateway auth and upstream auth passthrough are conceptually incompatible as sketched

The design asks clients to authenticate to the gateway with `Authorization: Bearer lab-token` and also expects `Authorization` to be forwarded to upstreams in passthrough mode. One header cannot safely carry both gateway credentials and upstream end-user credentials.

References:
- The passthrough test expectation is in `test.md:398-408`.
- The gateway consumes `Authorization` in `internal/middleware/auth.go:18-19`.

Impact: this requires a design decision, not just a code fix. For example, gateway authentication could use mTLS or a separate header, while upstream `Authorization` remains available for passthrough.

### P1 - Fan-out has no delivery contract

`fan_out_mode: any` returns success if one target succeeds, but the design has no retry queue, replay mechanism, target health state, idempotency strategy, or operator workflow for writes missed by failed targets.

References:
- The design expects partial success in `test.md:295-317`.
- `doAnyMode` returns the first success while reporting partial failures in `internal/fanout/fanout.go:152-183`.

Impact: partial success is data loss unless the design explicitly accepts eventual divergence. For observability data, that needs to be stated and backed by operational mechanics.

### P1 - The unified backend abstraction is only superficial

Loki, Mimir, and Tempo have different protocol shapes, auth expectations, query semantics, ingestion formats, and tenancy models. The current design unifies them mostly by URL prefix and a shared `InstanceConfig`, while backend-specific behavior is scattered through route handlers and rewrite code.

References:
- Shared fields live in `internal/config/config.go:52-61`.
- Backend-specific routes are hard-coded in `internal/fanout/loki.go`, `internal/fanout/mimir.go`, and `internal/fanout/tempo.go`.

Impact: adding more backend behavior will likely grow conditionals and inconsistent feature support. A clearer design would define backend capability contracts for push, query, tenant header behavior, rewrite support, and fan-out support.

### P1 - Tenantization is inconsistent across signals

Loki and Mimir can have label filtering/injection on writes, but Tempo explicitly rejects label config and has no equivalent span/resource attribute policy. Queries across all three signals are forwarded without a policy layer.

References:
- Tempo rejects labels in `internal/config/config.go:188-190`.
- Loki and Mimir push rewrites happen in `internal/fanout/loki.go:19-22` and `internal/fanout/mimir.go:19-21`.
- Tempo routes are pure proxy routes in `internal/fanout/tempo.go:12-40`.

Impact: the gateway does not provide a consistent tenantization story for logs, metrics, and traces. If the goal is unified tenant controls, each signal needs an explicit policy model.

### P1 - Secrets are modeled as plaintext configuration values

Backend `basic_auth` values and gateway tokens are ordinary YAML fields. There is no design for secret references, environment expansion, file references, secret rotation, redaction, or audit.

References:
- Secret-bearing config fields are in `internal/config/config.go:41-67`.
- Example plaintext credentials are present in `gateway.yaml:5`, `gateway.yaml:15-18`, `gateway.yaml:35-38`, `gateway.yaml:48`, and `gateway.yaml:54`.

Impact: this is fragile for production tenantization. The config should identify secret sources, not embed raw credential material that can be logged, returned, committed, or copied.

### P2 - No stated compatibility or versioning strategy for proxied APIs

The gateway exposes selected Loki, Mimir, and Tempo routes under its own path scheme, but the design does not define which upstream API versions are supported, how new endpoints are added, or how unsupported endpoints fail.

References:
- Loki, Mimir, and Tempo route subsets are hard-coded in `internal/fanout/loki.go`, `internal/fanout/mimir.go`, and `internal/fanout/tempo.go`.

Impact: clients may assume the gateway is a transparent replacement for each backend, but it is only a partial facade. That needs to be explicit to avoid silent product and operational gaps.

## Verification Notes

`go test ./cmd/gateway ./internal/config ./internal/middleware ./internal/metrics ./internal/rewrite` passes with `GOCACHE` redirected into `/private/tmp`.

`go test ./...` could not complete in this sandbox because tests using `httptest.NewServer` cannot bind local listener ports here, and the default Go cache path is outside the writable sandbox.
