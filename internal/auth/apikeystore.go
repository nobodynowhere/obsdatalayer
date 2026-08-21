package auth

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gofrs/uuid/v5"
	"gorm.io/gorm"

	dbstore "obsdatalayer/internal/db"
)

// lastUsedInterval is the minimum gap between last-used writes for one key.
//
// The field exists so an operator can tell which shipper credentials are still
// live and safely retire the rest. That needs a date, not a precise timestamp,
// so a busy key is not worth a database write per request.
const lastUsedInterval = time.Minute

// keyRecord is the in-memory form of a key, held so the data path can
// authenticate without a database round trip per request.
type keyRecord struct {
	id        string
	user      string
	hash      string
	expiresAt *time.Time

	// lastWrite is when last_used_at was last persisted, used to throttle.
	lastWrite time.Time
}

// apiKeyIndex is the authentication-side view of the key table, keyed by the
// clear-text handle.
type apiKeyIndex struct {
	mu   sync.RWMutex
	keys map[string]*keyRecord
}

func newAPIKeyIndex() *apiKeyIndex {
	return &apiKeyIndex{keys: make(map[string]*keyRecord)}
}

func (i *apiKeyIndex) replace(keys map[string]*keyRecord) {
	i.mu.Lock()
	defer i.mu.Unlock()
	// Carry the throttle state across a reload, or every reload would let the
	// next request write last_used_at again.
	for handle, rec := range keys {
		if prev, ok := i.keys[handle]; ok {
			rec.lastWrite = prev.lastWrite
		}
	}
	i.keys = keys
}

func (i *apiKeyIndex) lookup(handle string) (*keyRecord, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	rec, ok := i.keys[handle]
	return rec, ok
}

// markUsed reports whether last_used_at is due to be written for this key, and
// records the intent so concurrent requests do not all write at once.
func (i *apiKeyIndex) markUsed(handle string, now time.Time) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	rec, ok := i.keys[handle]
	if !ok {
		return false
	}
	if now.Sub(rec.lastWrite) < lastUsedInterval {
		return false
	}
	rec.lastWrite = now
	return true
}

// AuthenticateAPIKey resolves a bearer token to its owning user.
//
// Verification is a SHA-256 comparison against an in-memory snapshot, so it
// costs microseconds rather than the tens of milliseconds a password does. That
// is safe because the secret is 256 random bits: there is nothing to guess, so
// a deliberately slow hash would protect nothing while putting every shipper
// request on the expensive path.
func (s *Service) AuthenticateAPIKey(token string) (*User, error) {
	handle, rawSecret, err := parseAPIKey(token)
	if err != nil {
		return nil, ErrInvalidAPIKey
	}

	rec, ok := s.apiKeys.lookup(handle)
	if !ok {
		return nil, ErrInvalidAPIKey
	}
	if !verifyAPIKeySecret(rawSecret, rec.hash) {
		return nil, ErrInvalidAPIKey
	}

	now := time.Now()
	if rec.expiresAt != nil && now.After(*rec.expiresAt) {
		slog.Debug("api key rejected: expired", "user", rec.user, "handle", handle)
		return nil, ErrInvalidAPIKey
	}

	// The owner must still exist. A key outlives its user only between the
	// delete and the reload that follows it, but authenticating as a deleted
	// account for even that long is not acceptable.
	idx := *s.users.Load()
	u, ok := idx[rec.user]
	if !ok {
		return nil, ErrInvalidAPIKey
	}

	if s.apiKeys.markUsed(handle, now) {
		s.recordAPIKeyUse(rec.id, now)
	}
	return u, nil
}

// recordAPIKeyUse persists last_used_at. It runs inline rather than in a
// goroutine so a failure is visible in the request's own log line, and it is
// throttled by markUsed so this is at most one write per key per minute.
func (s *Service) recordAPIKeyUse(id string, at time.Time) {
	if err := s.db.Model(&dbstore.APIKey{}).
		Where("id = ?", id).
		Update("last_used_at", at).Error; err != nil {
		// Not fatal: the caller is authenticated either way, and losing a
		// last-used timestamp must never fail a request.
		slog.Debug("could not record api key use", "id", id, "error", err)
	}
}

// loadAPIKeys refreshes the in-memory key index from the database. Called by
// Reload, so creating or revoking a key takes effect immediately rather than at
// the next scheduled reload.
func (s *Service) loadAPIKeys() error {
	var rows []dbstore.APIKey
	if err := s.db.Find(&rows).Error; err != nil {
		return fmt.Errorf("auth: load api keys: %w", err)
	}
	keys := make(map[string]*keyRecord, len(rows))
	for _, row := range rows {
		keys[row.Handle] = &keyRecord{
			id:        row.ID.String(),
			user:      row.UserName,
			hash:      row.Hash,
			expiresAt: row.ExpiresAt,
		}
	}
	s.apiKeys.replace(keys)
	return nil
}

// CreateAPIKey issues a key for a user. The returned secret is the only copy.
func (s *Service) CreateAPIKey(user, label string, expiresAt *time.Time) (GeneratedAPIKey, error) {
	idx := *s.users.Load()
	if _, ok := idx[user]; !ok {
		return GeneratedAPIKey{}, ErrNotFound
	}
	if label == "" {
		return GeneratedAPIKey{}, errors.New("api key label is required")
	}
	if expiresAt != nil && !expiresAt.After(time.Now()) {
		return GeneratedAPIKey{}, errors.New("api key expiry must be in the future")
	}

	secret, handle, hash, err := generateAPIKey()
	if err != nil {
		return GeneratedAPIKey{}, err
	}
	id, err := uuid.NewV4()
	if err != nil {
		return GeneratedAPIKey{}, fmt.Errorf("generate api key id: %w", err)
	}

	row := dbstore.APIKey{
		ID:        id,
		UserName:  user,
		Label:     label,
		Handle:    handle,
		Hash:      hash,
		CreatedAt: time.Now(),
		ExpiresAt: expiresAt,
	}
	if err := s.db.Create(&row).Error; err != nil {
		return GeneratedAPIKey{}, fmt.Errorf("create api key: %w", err)
	}
	if err := s.Reload(); err != nil {
		return GeneratedAPIKey{}, err
	}

	return GeneratedAPIKey{
		APIKey: APIKey{
			ID:        row.ID.String(),
			User:      user,
			Label:     label,
			Handle:    handle,
			CreatedAt: row.CreatedAt,
			ExpiresAt: expiresAt,
		},
		Secret: secret,
	}, nil
}

// ListAPIKeys returns a user's keys as metadata. The secret is never returned:
// it exists only in the response that created it.
func (s *Service) ListAPIKeys(user string) ([]APIKey, error) {
	var rows []dbstore.APIKey
	if err := s.db.Where("user_name = ?", user).Order("created_at").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	out := make([]APIKey, 0, len(rows))
	for _, row := range rows {
		out = append(out, APIKey{
			ID:        row.ID.String(),
			User:      row.UserName,
			Label:     row.Label,
			Handle:    row.Handle,
			CreatedAt: row.CreatedAt,
			ExpiresAt: row.ExpiresAt,
			LastUsed:  row.LastUsedAt,
		})
	}
	return out, nil
}

// DeleteAPIKey revokes a key. The reload that follows drops it from the
// authentication index, so revocation takes effect at once rather than at the
// next scheduled reload.
func (s *Service) DeleteAPIKey(user, id string) error {
	res := s.db.Where("user_name = ? AND id = ?", user, id).Delete(&dbstore.APIKey{})
	if res.Error != nil {
		return fmt.Errorf("delete api key: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return s.Reload()
}

// deleteAPIKeysForUser removes every key belonging to a user, used when the
// user itself is deleted. Without it a key would outlive its owner in the
// table, and recreating the name would silently revive it.
func deleteAPIKeysForUser(db *gorm.DB, user string) error {
	if err := db.Where("user_name = ?", user).Delete(&dbstore.APIKey{}).Error; err != nil {
		return fmt.Errorf("delete api keys for %q: %w", user, err)
	}
	return nil
}
