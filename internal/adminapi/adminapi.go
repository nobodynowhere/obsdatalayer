// Package adminapi exposes user and role management over HTTP. Every route is
// intended to be mounted behind middleware.AdminAuth on the admin listener.
package adminapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"gorm.io/gorm"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/config"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/rewrite"
	"obsdatalayer/internal/secret"
	"obsdatalayer/internal/tenant"
)

// Deps are the collaborators the admin API needs. Reload is invoked after any
// mutation that changes routing or policy, so a change made through the API
// takes effect immediately rather than at the next scheduled reload.
type Deps struct {
	Auth        *auth.Service
	Tenants     *tenant.Store
	DB          *gorm.DB
	Config      *config.ConfigHolder
	Metrics     *metrics.Metrics
	MimirClient *http.Client
	Reload      func() error
	Cipher      *secret.Cipher
}

// Register mounts the tenant, user, role, instance and settings endpoints.
func Register(mux *http.ServeMux, d Deps) {
	h := &handler{svc: d.Auth, tenants: d.Tenants, db: d.DB, cfg: d.Config, metrics: d.Metrics, mimirClient: d.MimirClient, reload: d.Reload, cipher: d.Cipher}

	// Reads are registered directly; every mutation is wrapped so it emits a
	// started/finished pair naming the actor.
	mux.HandleFunc("GET /api/whoami", h.whoami)
	mux.HandleFunc("GET /api/metrics", h.getMetrics)
	mux.HandleFunc("GET /api/operational-endpoints", h.getOperationalCatalog)

	mux.HandleFunc("GET /api/instances", h.listInstances)
	mux.HandleFunc("GET /api/instances/{name}", h.getInstance)
	mux.HandleFunc("POST /api/instances", h.audited("instance.create", h.createInstance))
	mux.HandleFunc("PUT /api/instances/{name}", h.audited("instance.update", h.updateInstance))
	mux.HandleFunc("DELETE /api/instances/{name}", h.audited("instance.delete", h.deleteInstance))

	mux.HandleFunc("GET /api/settings", h.getSettings)
	mux.HandleFunc("PUT /api/settings", h.audited("settings.update", h.updateSettings))

	mux.HandleFunc("GET /api/tenants", h.listTenants)
	mux.HandleFunc("GET /api/tenants/{id}", h.getTenant)
	mux.HandleFunc("GET /api/tenants/{id}/mimir/observability", h.getTenantMimirObservability)
	mux.HandleFunc("POST /api/tenants", h.audited("tenant.create", h.createTenant))
	mux.HandleFunc("PUT /api/tenants/{id}", h.audited("tenant.update", h.updateTenant))
	mux.HandleFunc("DELETE /api/tenants/{id}", h.audited("tenant.delete", h.deleteTenant))

	mux.HandleFunc("GET /api/users", h.listUsers)
	mux.HandleFunc("GET /api/users/{name}", h.getUser)
	mux.HandleFunc("POST /api/users", h.audited("user.create", h.createUser))
	mux.HandleFunc("DELETE /api/users/{name}", h.audited("user.delete", h.deleteUser))
	mux.HandleFunc("PUT /api/users/{name}/password", h.audited("user.set_password", h.setPassword))
	mux.HandleFunc("PUT /api/users/{name}/roles", h.audited("user.set_roles", h.setUserRoles))
	mux.HandleFunc("PUT /api/users/{name}/grants", h.audited("user.set_grants", h.setUserGrants))

	// API keys are credentials for a user, so they hang off the user they
	// belong to. Issuing and revoking are mutations and are audited like the
	// rest; the secret itself never reaches the audit log, because only the
	// response carries it and bodies are recorded at debug with secrets masked.
	mux.HandleFunc("GET /api/users/{name}/apikeys", h.listAPIKeys)
	mux.HandleFunc("POST /api/users/{name}/apikeys", h.audited("user.apikey_create", h.createAPIKey))
	mux.HandleFunc("DELETE /api/users/{name}/apikeys/{id}", h.audited("user.apikey_delete", h.deleteAPIKey))

	mux.HandleFunc("GET /api/roles", h.listRoles)
	mux.HandleFunc("GET /api/roles/{name}", h.getRole)
	mux.HandleFunc("POST /api/roles", h.audited("role.create", h.createRole))
	mux.HandleFunc("DELETE /api/roles/{name}", h.audited("role.delete", h.deleteRole))
	mux.HandleFunc("PUT /api/roles/{name}/grants", h.audited("role.set_grants", h.setRoleGrants))
}

type handler struct {
	svc         *auth.Service
	tenants     *tenant.Store
	metrics     *metrics.Metrics
	db          *gorm.DB
	cfg         *config.ConfigHolder
	mimirClient *http.Client
	reload      func() error
	cipher      *secret.Cipher
}

// afterChange republishes configuration so a mutation is visible immediately.
func (h *handler) afterChange() error {
	if h.reload == nil {
		return nil
	}
	return h.reload()
}

// ---- request bodies ---------------------------------------------------------

type createUserReq struct {
	Name     string   `json:"name"`
	Password string   `json:"password"`
	Roles    []string `json:"roles"`
}

type passwordReq struct {
	Password string `json:"password"`
}

type rolesReq struct {
	Roles []string `json:"roles"`
}

type grantsReq struct {
	Grants []auth.Grant `json:"grants"`
}

type tenantReq struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// GrafanaID is optional; it is reserved for the future Grafana proxy.
	GrafanaID *int `json:"grafana_id"`
}

type createRoleReq struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Grants      []auth.Grant `json:"grants"`
}

// ---- users ------------------------------------------------------------------

func (h *handler) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.svc.ListUsers()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (h *handler) getUser(w http.ResponseWriter, r *http.Request) {
	user, err := h.svc.GetUser(r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (h *handler) createUser(w http.ResponseWriter, r *http.Request) {
	var req createUserReq
	if !decode(w, r, &req) {
		return
	}
	if err := h.svc.CreateUser(req.Name, req.Password, req.Roles); err != nil {
		writeErr(w, err)
		return
	}
	user, err := h.svc.GetUser(req.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, user)
}

func (h *handler) deleteUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	// Refuse to remove the last account that can still reach the admin plane,
	// which would otherwise lock everyone out with no recovery path.
	adminRemains, err := h.adminWouldRemain(func(s *adminSnapshot) {
		s.removeUser(name)
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	if !adminRemains {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "refusing to delete the last user with admin access",
		})
		return
	}

	if err := h.svc.DeleteUser(name); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) setPassword(w http.ResponseWriter, r *http.Request) {
	var req passwordReq
	if !decode(w, r, &req) {
		return
	}
	if err := h.svc.SetPassword(r.PathValue("name"), req.Password); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) setUserRoles(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req rolesReq
	if !decode(w, r, &req) {
		return
	}
	if _, err := h.svc.GetUser(name); err != nil {
		writeErr(w, err)
		return
	}

	adminRemains, err := h.adminWouldRemain(func(s *adminSnapshot) {
		s.setUserRoles(name, req.Roles)
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	if !adminRemains {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "refusing to remove admin access from the last admin user",
		})
		return
	}

	if err := h.svc.SetUserRoles(name, req.Roles); err != nil {
		writeErr(w, err)
		return
	}
	if err := h.svc.Reload(); err != nil {
		writeErr(w, err)
		return
	}
	user, err := h.svc.GetUser(name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (h *handler) setUserGrants(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req grantsReq
	if !decode(w, r, &req) {
		return
	}
	if _, err := h.svc.GetUser(name); err != nil {
		writeErr(w, err)
		return
	}
	if err := rewrite.ValidateGrantReadPolicies(req.Grants); err != nil {
		writeErr(w, err)
		return
	}
	adminRemains, err := h.adminWouldRemain(func(s *adminSnapshot) {
		s.setUserGrants(name, req.Grants)
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	if !adminRemains {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "refusing to remove admin access from the last admin user",
		})
		return
	}
	if err := h.svc.SetUserGrants(name, req.Grants); err != nil {
		writeErr(w, err)
		return
	}
	user, err := h.svc.GetUser(name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, user)
}

type adminSnapshot struct {
	users []auth.UserInfo
	roles []auth.RoleInfo
}

func (h *handler) adminWouldRemain(apply func(*adminSnapshot)) (bool, error) {
	users, err := h.svc.ListUsers()
	if err != nil {
		return false, err
	}
	roles, err := h.svc.ListRoles()
	if err != nil {
		return false, err
	}
	s := &adminSnapshot{users: users, roles: roles}
	apply(s)
	return s.hasAdmin(), nil
}

func (s *adminSnapshot) hasAdmin() bool {
	roleAdmins := make(map[string]bool, len(s.roles))
	for _, role := range s.roles {
		roleAdmins[role.Name] = grantsAdmin(role.Grants)
	}
	for _, user := range s.users {
		if grantsAdmin(user.Grants) {
			return true
		}
		for _, role := range user.Roles {
			if roleAdmins[role] {
				return true
			}
		}
	}
	return false
}

func (s *adminSnapshot) removeUser(name string) {
	dst := s.users[:0]
	for _, user := range s.users {
		if user.Name != name {
			dst = append(dst, user)
		}
	}
	s.users = dst
}

func (s *adminSnapshot) setUserRoles(name string, roles []string) {
	for i := range s.users {
		if s.users[i].Name == name {
			s.users[i].Roles = roles
			return
		}
	}
}

func (s *adminSnapshot) setUserGrants(name string, grants []auth.Grant) {
	for i := range s.users {
		if s.users[i].Name == name {
			s.users[i].Grants = grants
			return
		}
	}
}

func (s *adminSnapshot) removeRole(name string) {
	dst := s.roles[:0]
	for _, role := range s.roles {
		if role.Name != name {
			dst = append(dst, role)
		}
	}
	s.roles = dst
	for i := range s.users {
		filtered := s.users[i].Roles[:0]
		for _, role := range s.users[i].Roles {
			if role != name {
				filtered = append(filtered, role)
			}
		}
		s.users[i].Roles = filtered
	}
}

func (s *adminSnapshot) setRoleGrants(name string, grants []auth.Grant) {
	for i := range s.roles {
		if s.roles[i].Name == name {
			s.roles[i].Grants = grants
			return
		}
	}
}

// getMetrics returns the aggregated gateway counters for the overview page.
// The Prometheus exposition at /metrics stays the source of truth for scraping;
// this is a small JSON projection so the SPA does not have to parse text format.
func (h *handler) getMetrics(w http.ResponseWriter, r *http.Request) {
	if h.metrics == nil {
		writeJSON(w, http.StatusOK, metrics.Summary{Instances: []metrics.InstanceSummary{}})
		return
	}
	summary := h.metrics.Summary()
	if summary.Instances == nil {
		summary.Instances = []metrics.InstanceSummary{}
	}
	writeJSON(w, http.StatusOK, summary)
}

// whoami reports the authenticated principal. The UI calls it to validate
// credentials at sign-in and to render the current identity.
func (h *handler) whoami(w http.ResponseWriter, r *http.Request) {
	ra := auth.FromContext(r.Context())
	if ra == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	info, err := h.svc.GetUser(ra.Username)
	if err != nil {
		// Authenticated but no user record: report the identity we do have.
		writeJSON(w, http.StatusOK, map[string]any{
			"username": ra.Username,
			"admin":    h.svc.CanAdmin(ra.Username),
		})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// ---- tenants ----------------------------------------------------------------

func (h *handler) listTenants(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"tenants": h.tenants.List()})
}

func (h *handler) getTenant(w http.ResponseWriter, r *http.Request) {
	t, ok := h.tenants.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, tenant.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (h *handler) createTenant(w http.ResponseWriter, r *http.Request) {
	var req tenantReq
	if !decode(w, r, &req) {
		return
	}
	t, err := h.tenants.Create(req.ID, req.Name, req.GrafanaID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (h *handler) updateTenant(w http.ResponseWriter, r *http.Request) {
	var req tenantReq
	if !decode(w, r, &req) {
		return
	}
	t, err := h.tenants.Update(r.PathValue("id"), req.Name, req.GrafanaID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// deleteTenant refuses to remove a tenant that any grant still references,
// which would otherwise silently strip tenant scoping from those grants.
func (h *handler) deleteTenant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	users, err := h.svc.ListUsers()
	if err != nil {
		writeErr(w, err)
		return
	}
	roles, err := h.svc.ListRoles()
	if err != nil {
		writeErr(w, err)
		return
	}
	var refs []string
	for _, u := range users {
		if grantsReference(u.Grants, id) {
			refs = append(refs, "user "+u.Name)
		}
	}
	for _, ro := range roles {
		if grantsReference(ro.Grants, id) {
			refs = append(refs, "role "+ro.Name)
		}
	}
	if len(refs) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      "tenant is still referenced by existing grants",
			"references": refs,
		})
		return
	}

	if err := h.tenants.Delete(id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func grantsReference(grants []auth.Grant, tenantID string) bool {
	for _, g := range grants {
		for _, t := range g.TenantIDs {
			if t == tenantID {
				return true
			}
		}
	}
	return false
}

// ---- roles ------------------------------------------------------------------

func (h *handler) listRoles(w http.ResponseWriter, r *http.Request) {
	roles, err := h.svc.ListRoles()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"roles": roles})
}

func (h *handler) getRole(w http.ResponseWriter, r *http.Request) {
	role, err := h.svc.GetRole(r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, role)
}

func (h *handler) createRole(w http.ResponseWriter, r *http.Request) {
	var req createRoleReq
	if !decode(w, r, &req) {
		return
	}
	if err := rewrite.ValidateGrantReadPolicies(req.Grants); err != nil {
		writeErr(w, err)
		return
	}
	if err := h.svc.CreateRole(req.Name, req.Description, req.Grants); err != nil {
		writeErr(w, err)
		return
	}
	role, err := h.svc.GetRole(req.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, role)
}

func (h *handler) deleteRole(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == auth.RoleAdmin {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "the built-in admin role cannot be deleted",
		})
		return
	}
	if _, err := h.svc.GetRole(name); err != nil {
		writeErr(w, err)
		return
	}
	adminRemains, err := h.adminWouldRemain(func(s *adminSnapshot) {
		s.removeRole(name)
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	if !adminRemains {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "refusing to remove the last admin access path",
		})
		return
	}
	if err := h.svc.DeleteRole(name); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) setRoleGrants(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req grantsReq
	if !decode(w, r, &req) {
		return
	}
	if name == auth.RoleAdmin && !grantsAdmin(req.Grants) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "the built-in admin role must retain admin access",
		})
		return
	}
	if err := rewrite.ValidateGrantReadPolicies(req.Grants); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := h.svc.GetRole(name); err != nil {
		writeErr(w, err)
		return
	}
	adminRemains, err := h.adminWouldRemain(func(s *adminSnapshot) {
		s.setRoleGrants(name, req.Grants)
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	if !adminRemains {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "refusing to remove the last admin access path",
		})
		return
	}
	if err := h.svc.SetRoleGrants(name, req.Grants); err != nil {
		writeErr(w, err)
		return
	}
	role, err := h.svc.GetRole(name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, role)
}

func grantsAdmin(grants []auth.Grant) bool {
	for _, g := range grants {
		if g.Backend == auth.ObjectAdmin && g.Action == auth.ActionAccess {
			return true
		}
	}
	return false
}

// ---- helpers ----------------------------------------------------------------

// maxBodyBytes caps admin request bodies; these are small JSON documents.
const maxBodyBytes = 1 << 20

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidGrant):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, auth.ErrNotFound), errors.Is(err, tenant.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case errors.Is(err, auth.ErrExists), errors.Is(err, tenant.ErrExists):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "already exists"})
	case errors.Is(err, tenant.ErrInUse):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, config.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case errors.Is(err, config.ErrExists):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "already exists"})
	case errors.Is(err, errAmbiguousMimirInstance):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
}
