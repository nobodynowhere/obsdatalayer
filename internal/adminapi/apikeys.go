package adminapi

import (
	"net/http"
	"time"
)

// ---- API keys ---------------------------------------------------------------
//
// A key is a bearer credential belonging to a user, inheriting that user's
// grants. These endpoints manage the credential; authorization is unchanged and
// still lives entirely in grants.

type apiKeyRequest struct {
	Label string `json:"label"`
	// ExpiresAt is RFC 3339 and optional. Omitted or empty means the key does
	// not expire, which is the default for unattended shippers.
	ExpiresAt string `json:"expires_at,omitempty"`
}

func (h *handler) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := h.svc.GetUser(name); err != nil {
		writeErr(w, err)
		return
	}
	keys, err := h.svc.ListAPIKeys(name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, keys)
}

// createAPIKey issues a key. The response carries the only copy of the secret;
// it is not recoverable afterwards, because only its hash is stored.
func (h *handler) createAPIKey(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req apiKeyRequest
	if !decode(w, r, &req) {
		return
	}

	var expiresAt *time.Time
	if req.ExpiresAt != "" {
		parsed, err := time.Parse(time.RFC3339, req.ExpiresAt)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "expires_at must be an RFC 3339 timestamp",
			})
			return
		}
		expiresAt = &parsed
	}

	key, err := h.svc.CreateAPIKey(name, req.Label, expiresAt)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := h.afterChange(); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, key)
}

func (h *handler) deleteAPIKey(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	id := r.PathValue("id")
	if err := h.svc.DeleteAPIKey(name, id); err != nil {
		writeErr(w, err)
		return
	}
	if err := h.afterChange(); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
