package adminapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"obsdatalayer/internal/adminapi"
	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/config"
	"obsdatalayer/internal/db"
	"obsdatalayer/internal/tenant"
)

type env struct {
	mux     *http.ServeMux
	svc     *auth.Service
	tenants *tenant.Store
	cfg     *config.ConfigHolder
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	gormDB, err := db.Open(db.Config{Type: "sqlite", Path: dsn})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Migrate(gormDB); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gormDB.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	tenants, err := tenant.NewStore(gormDB)
	if err != nil {
		t.Fatalf("tenant store: %v", err)
	}
	svc, err := auth.NewService(gormDB, tenants)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}

	if err := config.EnsureSettings(gormDB); err != nil {
		t.Fatalf("ensure settings: %v", err)
	}
	holder, err := config.NewDBHolder(gormDB, "test")
	if err != nil {
		t.Fatalf("config holder: %v", err)
	}
	reload := func() error {
		if _, err := holder.Reload(); err != nil {
			return err
		}
		return tenants.Reload()
	}

	mux := http.NewServeMux()
	adminapi.Register(mux, adminapi.Deps{
		Auth: svc, Tenants: tenants, DB: gormDB, Config: holder, Reload: reload,
	})
	return &env{mux: mux, svc: svc, tenants: tenants, cfg: holder}
}

// do issues a request against the admin mux. The mux is mounted without
// AdminAuth here; authentication is covered by the middleware tests.
func (e *env) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func decodeInto(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
}

// ---- tenants ----------------------------------------------------------------

func TestCreateAndListTenants(t *testing.T) {
	e := newEnv(t)

	rec := e.do(t, http.MethodPost, "/api/tenants", map[string]any{"name": "acme"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body)
	}
	var created tenant.Tenant
	decodeInto(t, rec, &created)
	if created.ID == "" {
		t.Fatal("expected a generated tenant UUID")
	}
	if created.GrafanaID != nil {
		t.Errorf("grafana_id should be unset by default, got %v", *created.GrafanaID)
	}

	rec = e.do(t, http.MethodGet, "/api/tenants", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var listed struct {
		Tenants []tenant.Tenant `json:"tenants"`
	}
	decodeInto(t, rec, &listed)
	if len(listed.Tenants) != 1 || listed.Tenants[0].Name != "acme" {
		t.Errorf("unexpected tenant list: %+v", listed.Tenants)
	}
}

func TestCreateTenantWithGrafanaID(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, http.MethodPost, "/api/tenants", map[string]any{"name": "acme", "grafana_id": 42})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body)
	}
	var created tenant.Tenant
	decodeInto(t, rec, &created)
	if created.GrafanaID == nil || *created.GrafanaID != 42 {
		t.Errorf("expected grafana_id 42, got %v", created.GrafanaID)
	}
}

func TestGetMissingTenant(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, http.MethodGet, "/api/tenants/6f1d2c9e-9d3a-4c1b-8f47-2b0a5e7c1d34", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

// Deleting a tenant that grants still reference would silently strip tenant
// scoping from those grants, so it must be refused.
func TestDeleteTenantInUseIsRefused(t *testing.T) {
	e := newEnv(t)

	created, err := e.tenants.Create("", "acme", nil)
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := e.svc.CreateRole("reader", "", []auth.Grant{{
		Backend: "loki", Action: "read", TenantIDs: []string{created.ID},
	}}); err != nil {
		t.Fatalf("create role: %v", err)
	}

	rec := e.do(t, http.MethodDelete, "/api/tenants/"+created.ID, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body)
	}
	if !e.tenants.Exists(created.ID) {
		t.Error("tenant must still exist after a refused delete")
	}
}

func TestDeleteUnreferencedTenant(t *testing.T) {
	e := newEnv(t)
	created, err := e.tenants.Create("", "acme", nil)
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	rec := e.do(t, http.MethodDelete, "/api/tenants/"+created.ID, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body)
	}
	if e.tenants.Exists(created.ID) {
		t.Error("expected the tenant to be deleted")
	}
}

// ---- users and roles --------------------------------------------------------

func TestCreateUserWithRole(t *testing.T) {
	e := newEnv(t)
	tn, err := e.tenants.Create("", "acme", nil)
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	rec := e.do(t, http.MethodPost, "/api/roles", map[string]any{
		"name": "reader",
		"grants": []map[string]any{
			{"backend": "loki", "action": "read", "tenant_ids": []string{tn.ID}},
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create role: expected 201, got %d: %s", rec.Code, rec.Body)
	}

	rec = e.do(t, http.MethodPost, "/api/users", map[string]any{
		"name": "alice", "password": "correct-horse-battery-staple", "roles": []string{"reader"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user: expected 201, got %d: %s", rec.Code, rec.Body)
	}

	ids, ok := e.svc.TenantIDsFor("alice", "loki", "read")
	if !ok {
		t.Fatal("expected the role to grant loki:read")
	}
	if len(ids) != 1 || ids[0] != tn.ID {
		t.Errorf("expected [%s], got %v", tn.ID, ids)
	}
}

func TestCreateRoleRejectsUnknownTenant(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, http.MethodPost, "/api/roles", map[string]any{
		"name": "reader",
		"grants": []map[string]any{
			{"backend": "loki", "action": "read",
				"tenant_ids": []string{"6f1d2c9e-9d3a-4c1b-8f47-2b0a5e7c1d34"}},
		},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for a grant naming an unregistered tenant, got %d: %s", rec.Code, rec.Body)
	}
}

func TestCreateRoleRejectsDifferentWriteTenants(t *testing.T) {
	e := newEnv(t)
	tenantA, err := e.tenants.Create("", "acme", nil)
	if err != nil {
		t.Fatalf("create tenant acme: %v", err)
	}
	tenantB, err := e.tenants.Create("", "globex", nil)
	if err != nil {
		t.Fatalf("create tenant globex: %v", err)
	}

	rec := e.do(t, http.MethodPost, "/api/roles", map[string]any{
		"name": "writers",
		"grants": []map[string]any{
			{"backend": "loki", "action": "write", "tenant_ids": []string{tenantA.ID}},
			{"backend": "mimir", "action": "write", "tenant_ids": []string{tenantB.ID}},
		},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body)
	}
}

func TestCreateRoleWithMimirReadPolicy(t *testing.T) {
	e := newEnv(t)
	tn, err := e.tenants.Create("", "acme", nil)
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	rec := e.do(t, http.MethodPost, "/api/roles", map[string]any{
		"name": "metrics-reader",
		"grants": []map[string]any{
			{
				"backend":             "mimir",
				"action":              "read",
				"tenant_ids":          []string{tn.ID},
				"read_label_selector": ` {cluster="prod"} `,
			},
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body)
	}
	var got auth.RoleInfo
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode role: %v", err)
	}
	if len(got.Grants) != 1 || got.Grants[0].ReadLabelSelector != `{cluster="prod"}` {
		t.Fatalf("expected role grant read policy, got %+v", got.Grants)
	}
}

func TestCreateRoleRejectsInvalidMimirReadPolicy(t *testing.T) {
	e := newEnv(t)
	tn, err := e.tenants.Create("", "acme", nil)
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	rec := e.do(t, http.MethodPost, "/api/roles", map[string]any{
		"name": "metrics-reader",
		"grants": []map[string]any{
			{
				"backend":             "mimir",
				"action":              "read",
				"tenant_ids":          []string{tn.ID},
				"read_label_selector": `{cluster=`,
			},
		},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body)
	}
}

func TestCreateUserRejectsShortPassword(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, http.MethodPost, "/api/users", map[string]any{"name": "alice", "password": "short"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body)
	}
}

func TestDuplicateUserConflicts(t *testing.T) {
	e := newEnv(t)
	body := map[string]any{"name": "alice", "password": "correct-horse-battery-staple"}
	if rec := e.do(t, http.MethodPost, "/api/users", body); rec.Code != http.StatusCreated {
		t.Fatalf("first create: %d", rec.Code)
	}
	if rec := e.do(t, http.MethodPost, "/api/users", body); rec.Code != http.StatusConflict {
		t.Errorf("expected 409 on duplicate, got %d", rec.Code)
	}
}

func TestUnknownFieldsRejected(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/api/tenants",
		bytes.NewBufferString(`{"name":"acme","typo":true}`))
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for an unknown field, got %d", rec.Code)
	}
}

// ---- admin lockout guards ---------------------------------------------------

// Removing the only account that can reach the admin plane would leave the
// gateway unmanageable with no recovery path.
func TestCannotDeleteLastAdmin(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.EnsureBootstrapAdmin(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	rec := e.do(t, http.MethodDelete, "/api/users/admin", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body)
	}
	if !e.svc.CanAdmin("admin") {
		t.Error("admin must still have access after a refused delete")
	}
}

func TestCannotStripRolesFromLastAdmin(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.EnsureBootstrapAdmin(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	rec := e.do(t, http.MethodPut, "/api/users/admin/roles", map[string]any{"roles": []string{}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body)
	}
	if !e.svc.CanAdmin("admin") {
		t.Error("admin must still have access after a refused role change")
	}
}

// Once a second admin exists the guard should step out of the way.
func TestCanDeleteAdminWhenAnotherExists(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.EnsureBootstrapAdmin(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := e.svc.CreateUser("second", "correct-horse-battery-staple", []string{auth.RoleAdmin}); err != nil {
		t.Fatalf("create second admin: %v", err)
	}

	rec := e.do(t, http.MethodDelete, "/api/users/admin", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body)
	}
	if !e.svc.CanAdmin("second") {
		t.Error("the remaining admin should still have access")
	}
}

func TestBuiltInAdminRoleIsProtected(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.EnsureBootstrapAdmin(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	if rec := e.do(t, http.MethodDelete, "/api/roles/admin", nil); rec.Code != http.StatusConflict {
		t.Errorf("expected 409 deleting the built-in admin role, got %d", rec.Code)
	}

	rec := e.do(t, http.MethodPut, "/api/roles/admin/grants", map[string]any{
		"grants": []map[string]any{},
	})
	if rec.Code != http.StatusConflict {
		t.Errorf("expected 409 stripping admin access from the admin role, got %d", rec.Code)
	}
	if !e.svc.CanAdmin("admin") {
		t.Error("admin access must survive a refused grant change")
	}
}

func TestCannotStripDirectGrantsFromLastAdmin(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.CreateUser("solo", "correct-horse-battery-staple", nil); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := e.svc.SetUserGrants("solo", []auth.Grant{{Backend: auth.ObjectAdmin, Action: auth.ActionAccess}}); err != nil {
		t.Fatalf("grant admin: %v", err)
	}

	rec := e.do(t, http.MethodPut, "/api/users/solo/grants", map[string]any{
		"grants": []map[string]any{},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 stripping the last direct admin grant, got %d: %s", rec.Code, rec.Body)
	}
	if !e.svc.CanAdmin("solo") {
		t.Error("admin access must survive a refused direct grant change")
	}
}

func TestCannotDeleteCustomLastAdminRole(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.CreateRole("ops", "", []auth.Grant{{Backend: auth.ObjectAdmin, Action: auth.ActionAccess}}); err != nil {
		t.Fatalf("create role: %v", err)
	}
	if err := e.svc.CreateUser("solo", "correct-horse-battery-staple", []string{"ops"}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	rec := e.do(t, http.MethodDelete, "/api/roles/ops", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 deleting the last custom admin role, got %d: %s", rec.Code, rec.Body)
	}
	if !e.svc.CanAdmin("solo") {
		t.Error("admin access must survive a refused custom role delete")
	}
}

func TestCannotStripCustomLastAdminRole(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.CreateRole("ops", "", []auth.Grant{{Backend: auth.ObjectAdmin, Action: auth.ActionAccess}}); err != nil {
		t.Fatalf("create role: %v", err)
	}
	if err := e.svc.CreateUser("solo", "correct-horse-battery-staple", []string{"ops"}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	rec := e.do(t, http.MethodPut, "/api/roles/ops/grants", map[string]any{
		"grants": []map[string]any{},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 stripping the last custom admin role, got %d: %s", rec.Code, rec.Body)
	}
	if !e.svc.CanAdmin("solo") {
		t.Error("admin access must survive a refused custom role grant change")
	}
}

// ---- audit logging ----------------------------------------------------------

// captureLogs redirects slog to a buffer for the duration of a test.
func captureLogs(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// Every mutation emits a started/finished pair naming the operation.
func TestMutationsAreAudited(t *testing.T) {
	e := newEnv(t)
	logs := captureLogs(t, slog.LevelInfo)

	rec := e.do(t, http.MethodPost, "/api/tenants", map[string]any{"name": "acme"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}

	out := logs.String()
	for _, want := range []string{
		"admin operation started",
		"admin operation completed",
		"op=tenant.create",
		"status=201",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected the audit log to contain %q, got:\n%s", want, out)
		}
	}
}

// A rejected mutation is logged as failed, at warn, with the status.
func TestFailedMutationIsAuditedAsFailure(t *testing.T) {
	e := newEnv(t)
	logs := captureLogs(t, slog.LevelInfo)

	rec := e.do(t, http.MethodPost, "/api/users", map[string]any{"name": "alice", "password": "short"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}

	out := logs.String()
	if !strings.Contains(out, "admin operation failed") {
		t.Errorf("expected a failure line, got:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("expected the failure to log at warn, got:\n%s", out)
	}
}

// Reads are not audited: they would drown the useful lines.
func TestReadsAreNotAudited(t *testing.T) {
	e := newEnv(t)
	logs := captureLogs(t, slog.LevelInfo)

	e.do(t, http.MethodGet, "/api/tenants", nil)
	if strings.Contains(logs.String(), "admin operation") {
		t.Errorf("reads should not emit audit lines, got:\n%s", logs.String())
	}
}

// The request body is recorded at debug so an operator can see what was
// submitted -- but credentials must never reach the log.
func TestAuditBodyRedactsSecrets(t *testing.T) {
	e := newEnv(t)
	logs := captureLogs(t, slog.LevelDebug)

	const secret = "correct-horse-battery-staple"
	e.do(t, http.MethodPost, "/api/users", map[string]any{"name": "alice", "password": secret})

	out := logs.String()
	if strings.Contains(out, secret) {
		t.Fatalf("password leaked into the audit log:\n%s", out)
	}
	if !strings.Contains(out, "admin operation request body") {
		t.Errorf("expected the body to be logged at debug, got:\n%s", out)
	}
	if !strings.Contains(out, "alice") {
		t.Errorf("expected non-sensitive fields to survive redaction, got:\n%s", out)
	}
}

// Nested and per-target credentials are redacted too, not just top-level ones.
func TestAuditRedactsNestedSecrets(t *testing.T) {
	e := newEnv(t)
	logs := captureLogs(t, slog.LevelDebug)

	e.do(t, http.MethodPost, "/api/instances", map[string]any{
		"name": "mimir-prod", "backend": "mimir",
		"push_urls": []map[string]any{
			{"url": "http://a.local", "basic_auth": "user:topsecret"},
		},
	})

	out := logs.String()
	if strings.Contains(out, "topsecret") {
		t.Fatalf("nested basic_auth leaked into the audit log:\n%s", out)
	}
	if !strings.Contains(out, "http://a.local") {
		t.Errorf("expected non-sensitive nested fields to survive, got:\n%s", out)
	}
}

func TestCreateInstancePersistsSkipTLSVerify(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, http.MethodPost, "/api/instances", map[string]any{
		"name":            "mimir-prod",
		"backend":         "mimir",
		"url":             "https://mimir.local",
		"skip_tls_verify": true,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body)
	}
	inst := e.cfg.Get().ByName["mimir-prod"]
	if inst == nil || !inst.SkipTLSVerify {
		t.Fatalf("expected skip_tls_verify to be persisted, got %+v", inst)
	}
	var body map[string]any
	decodeInto(t, rec, &body)
	if body["skip_tls_verify"] != true {
		t.Errorf("expected API response to include skip_tls_verify=true, got %+v", body)
	}
}

// A body the handler will reject must not be echoed raw, or a malformed
// document could smuggle a credential into the log.
func TestAuditDoesNotEchoUnparsableBody(t *testing.T) {
	e := newEnv(t)
	logs := captureLogs(t, slog.LevelDebug)

	req := httptest.NewRequest(http.MethodPost, "/api/tenants",
		bytes.NewBufferString("this is not json: hunter2"))
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)

	out := logs.String()
	if strings.Contains(out, "hunter2") {
		t.Fatalf("unparsable body was echoed into the log:\n%s", out)
	}
	if !strings.Contains(out, "unparsable body omitted") {
		t.Errorf("expected the body to be reported as unparsable, got:\n%s", out)
	}
}

// Audit buffering must not break the handler's ability to read the body.
func TestAuditPreservesRequestBody(t *testing.T) {
	e := newEnv(t)
	captureLogs(t, slog.LevelDebug)

	rec := e.do(t, http.MethodPost, "/api/tenants", map[string]any{"name": "acme", "grafana_id": 9})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body)
	}
	var created tenant.Tenant
	decodeInto(t, rec, &created)
	if created.Name != "acme" || created.GrafanaID == nil || *created.GrafanaID != 9 {
		t.Errorf("body did not survive audit buffering: %+v", created)
	}
}

// A create names its subject even though the name arrives in the body.
func TestAuditNamesCreatedSubject(t *testing.T) {
	e := newEnv(t)
	logs := captureLogs(t, slog.LevelInfo)

	e.do(t, http.MethodPost, "/api/tenants", map[string]any{"name": "acme"})

	if !strings.Contains(logs.String(), "target=acme") {
		t.Errorf("expected the created subject to be named, got:\n%s", logs.String())
	}
}
