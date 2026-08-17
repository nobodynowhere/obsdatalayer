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
	"obsdatalayer/internal/tenant"
)

// Deps are the collaborators the admin API needs. Reload is invoked after any
// mutation that changes routing or policy, so a change made through the API
// takes effect immediately rather than at the next scheduled reload.
type Deps struct {
	Auth    *auth.Service
	Tenants *tenant.Store
	DB      *gorm.DB
	Config  *config.ConfigHolder
	Reload  func() error
}

// Register mounts the tenant, user, role, instance and settings endpoints.
func Register(mux *http.ServeMux, d Deps) {
	h := &handler{svc: d.Auth, tenants: d.Tenants, db: d.DB, cfg: d.Config, reload: d.Reload}

	// Reads are registered directly; every mutation is wrapped so it emits a
	// started/finished pair naming the actor.
	mux.HandleFunc("GET /whoami", h.whoami)

	mux.HandleFunc("GET /instances", h.listInstances)
	mux.HandleFunc("GET /instances/{name}", h.getInstance)
	mux.HandleFunc("POST /instances", h.audited("instance.create", h.createInstance))
	mux.HandleFunc("PUT /instances/{name}", h.audited("instance.update", h.updateInstance))
	mux.HandleFunc("DELETE /instances/{name}", h.audited("instance.delete", h.deleteInstance))

	mux.HandleFunc("GET /settings", h.getSettings)
	mux.HandleFunc("PUT /settings", h.audited("settings.update", h.updateSettings))

	mux.HandleFunc("GET /tenants", h.listTenants)
	mux.HandleFunc("GET /tenants/{id}", h.getTenant)
	mux.HandleFunc("POST /tenants", h.audited("tenant.create", h.createTenant))
	mux.HandleFunc("PUT /tenants/{id}", h.audited("tenant.update", h.updateTenant))
	mux.HandleFunc("DELETE /tenants/{id}", h.audited("tenant.delete", h.deleteTenant))

	mux.HandleFunc("GET /users", h.listUsers)
	mux.HandleFunc("GET /users/{name}", h.getUser)
	mux.HandleFunc("POST /users", h.audited("user.create", h.createUser))
	mux.HandleFunc("DELETE /users/{name}", h.audited("user.delete", h.deleteUser))
	mux.HandleFunc("PUT /users/{name}/password", h.audited("user.set_password", h.setPassword))
	mux.HandleFunc("PUT /users/{name}/roles", h.audited("user.set_roles", h.setUserRoles))
	mux.HandleFunc("PUT /users/{name}/grants", h.audited("user.set_grants", h.setUserGrants))

	mux.HandleFunc("GET /roles", h.listRoles)
	mux.HandleFunc("GET /roles/{name}", h.getRole)
	mux.HandleFunc("POST /roles", h.audited("role.create", h.createRole))
	mux.HandleFunc("DELETE /roles/{name}", h.audited("role.delete", h.deleteRole))
	mux.HandleFunc("PUT /roles/{name}/grants", h.audited("role.set_grants", h.setRoleGrants))
}

type handler struct {
	svc     *auth.Service
	tenants *tenant.Store
	db      *gorm.DB
	cfg     *config.ConfigHolder
	reload  func() error
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
	locksOut, err := h.lastAdmin(name)
	if err != nil {
		writeErr(w, err)
		return
	}
	if locksOut {
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

	// Same lockout guard as delete: stripping admin from the last admin would
	// leave the admin plane unreachable.
	locksOut, err := h.lastAdmin(name)
	if err != nil {
		writeErr(w, err)
		return
	}
	if locksOut && !containsAdminRole(req.Roles) {
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

// lastAdmin reports whether name is the only remaining user with admin access.
func (h *handler) lastAdmin(name string) (bool, error) {
	users, err := h.svc.ListUsers()
	if err != nil {
		return false, err
	}
	var target, others bool
	for _, u := range users {
		if !u.Admin {
			continue
		}
		if u.Name == name {
			target = true
		} else {
			others = true
		}
	}
	return target && !others, nil
}

func containsAdminRole(roles []string) bool {
	for _, r := range roles {
		if r == auth.RoleAdmin {
			return true
		}
	}
	return false
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
		if g.IsAdmin() {
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
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
}
