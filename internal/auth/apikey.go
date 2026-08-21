package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// API keys are bearer credentials belonging to a user.
//
// A key is a credential, not a new authorization concept: authenticating with
// one is authenticating as its owner, and the owner's grants decide everything
// that follows. Narrowing what a key may do means creating a narrower user,
// which is already how service accounts work here. That keeps one authorization
// model rather than two that can drift apart.
//
// This is not the global bearer token that earlier revisions removed. That was
// a single shared secret with no identity behind it; every holder was the same
// anonymous principal. A key here names exactly one user and is revoked on its
// own without disturbing anything else.
const (
	// keyPrefixLabel marks the token so a leaked one is recognisable in a log
	// or a commit, and so a secret scanner can be taught to spot it.
	keyPrefixLabel = "obsgw"

	// keyHandleBytes is the length of the lookup handle. It is stored in clear
	// and is not a secret: it exists so a presented key can be found without
	// scanning every row and hashing against each.
	keyHandleBytes = 6

	// keySecretBytes is the entropy that actually authenticates. 256 bits is
	// far beyond guessing, which is what makes a fast hash sound below.
	keySecretBytes = 32
)

// ErrInvalidAPIKey covers every way a presented key fails: malformed, unknown,
// expired or wrong. The caller learns only that it did not work, so a probe
// cannot distinguish "no such key" from "wrong secret".
var ErrInvalidAPIKey = errors.New("invalid API key")

// APIKey is the metadata of an issued key. The secret itself is never stored
// and appears exactly once, in the response that creates it.
type APIKey struct {
	ID        string     `json:"id"`
	User      string     `json:"user"`
	Label     string     `json:"label"`
	Handle    string     `json:"handle"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	LastUsed  *time.Time `json:"last_used_at,omitempty"`
}

// Expired reports whether the key is past its expiry. A key with no expiry
// never is: an unattended shipper whose credential lapses on a date nobody
// remembers is an outage, so expiry is opt-in.
func (k APIKey) Expired(now time.Time) bool {
	return k.ExpiresAt != nil && now.After(*k.ExpiresAt)
}

// GeneratedAPIKey is a newly issued key: the metadata, plus the one and only
// copy of the secret.
type GeneratedAPIKey struct {
	APIKey
	// Secret is the full token to hand to the client. It cannot be recovered
	// afterwards, because only its hash is kept.
	Secret string `json:"secret"`
}

// generateAPIKey mints a token and returns it alongside the handle and hash to
// store.
func generateAPIKey() (secret, handle, hash string, err error) {
	handleBytes := make([]byte, keyHandleBytes)
	if _, err := rand.Read(handleBytes); err != nil {
		return "", "", "", fmt.Errorf("generate key handle: %w", err)
	}
	secretBytes := make([]byte, keySecretBytes)
	if _, err := rand.Read(secretBytes); err != nil {
		return "", "", "", fmt.Errorf("generate key secret: %w", err)
	}

	handle = hex.EncodeToString(handleBytes)
	rawSecret := base64.RawURLEncoding.EncodeToString(secretBytes)
	secret = keyPrefixLabel + "_" + handle + "_" + rawSecret
	return secret, handle, hashAPIKeySecret(rawSecret), nil
}

// hashAPIKeySecret hashes the secret half of a token.
//
// SHA-256, deliberately, where user passwords use bcrypt. bcrypt is slow to
// make a guessable secret expensive to attack; this secret is 256 random bits,
// so there is nothing to guess and slowness would buy nothing. It would cost
// something, though: every shipper request would pay tens of milliseconds of
// CPU, which is the denial-of-service the gateway already has to defend the
// password path against.
func hashAPIKeySecret(rawSecret string) string {
	sum := sha256.Sum256([]byte(rawSecret))
	return hex.EncodeToString(sum[:])
}

// parseAPIKey splits a presented token into its handle and secret halves.
//
// The split is limited to three parts on purpose. The secret is base64url, and
// that alphabet contains "_", so splitting on every separator tore roughly half
// of all issued keys into four pieces and rejected them as malformed. The
// secret is everything after the second separator, underscores included.
func parseAPIKey(token string) (handle, rawSecret string, err error) {
	parts := strings.SplitN(strings.TrimSpace(token), "_", 3)
	if len(parts) != 3 || parts[0] != keyPrefixLabel || parts[1] == "" || parts[2] == "" {
		return "", "", ErrInvalidAPIKey
	}
	return parts[1], parts[2], nil
}

// verifyAPIKeySecret compares a presented secret against a stored hash without
// leaking how far the comparison got.
func verifyAPIKeySecret(rawSecret, storedHash string) bool {
	got := hashAPIKeySecret(rawSecret)
	return subtle.ConstantTimeCompare([]byte(got), []byte(storedHash)) == 1
}

// BearerToken extracts the token from an Authorization header value, reporting
// whether the header carried one at all.
func BearerToken(header string) (string, bool) {
	const scheme = "bearer "
	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	token := strings.TrimSpace(header[len(scheme):])
	if token == "" {
		return "", false
	}
	return token, true
}
