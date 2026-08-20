// Package secret encrypts credentials that must be stored recoverably.
//
// Gateway user passwords are bcrypt hashed and never recovered. Upstream
// backend credentials are different: the gateway replays them on every proxied
// request, so they cannot be hashed. They are encrypted at rest instead, with a
// key that lives outside the database. That is the whole point of the exercise
// -- a database backup or a read-only database credential must not be enough to
// read them.
//
// A nil *Cipher means encryption is not configured. Encrypt and Decrypt are
// written to work on a nil receiver so that call sites stay free of branches;
// the nil cipher passes plaintext through and refuses ciphertext it cannot
// possibly read. Whether a nil cipher is tolerable at all is decided at startup
// by config.EnsureCredentialsEncrypted, not here.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the required key length. AES-256 takes a 32-byte key.
const KeySize = 32

// envelopePrefix marks a stored value as ciphertext produced by this package.
// The version is part of the prefix so a future scheme can be introduced
// without ambiguity: an old value keeps its old prefix and stays readable.
const envelopePrefix = "enc:v1:"

// ErrNoCipher is returned when ciphertext is found but no key is configured.
var ErrNoCipher = errors.New("value is encrypted but no encryption key is configured")

// Cipher encrypts and decrypts credential values with AES-256-GCM.
type Cipher struct {
	aead cipher.AEAD
}

// New builds a Cipher from a raw 32-byte key.
func New(key []byte) (*Cipher, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("encryption key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("build cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("build GCM: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// IsEncrypted reports whether a stored value carries the envelope prefix.
//
// A credential is a "user:password" pair, so a plaintext value that happens to
// begin with "enc:v1:" would need a username of "enc" and a password beginning
// "v1:". Such a value would be misread as ciphertext and fail to decrypt, which
// is a loud failure rather than a silent one.
func IsEncrypted(stored string) bool {
	return strings.HasPrefix(stored, envelopePrefix)
}

// Encrypt seals a value for storage. Empty values are left empty: "no
// credential" is not a secret and encrypting it would only obscure the fact
// that the column is unset.
//
// Every non-empty value is sealed, including one that happens to begin with the
// envelope prefix. This used to short-circuit on IsEncrypted to make the call
// idempotent, which was a hole: a caller submitting the literal credential
// "enc:v1:hunter2" had it written to the database verbatim, in plaintext, with
// a key configured. The prefix describes what the gateway itself wrote; it is
// never evidence about a value arriving from outside, and treating it as such
// on the write path let untrusted input decide whether it was encrypted.
//
// Nothing needs the idempotence. The one caller that could pass an already
// sealed value, the startup migration in config.EnsureCredentialsEncrypted,
// partitions on IsEncrypted first and only ever hands this function plaintext.
//
// A nil Cipher returns the plaintext unchanged.
func (c *Cipher) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if c == nil {
		return plaintext, nil
	}

	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return envelopePrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt opens a stored value. A value without the envelope prefix is returned
// unchanged: it predates encryption, and tolerating it is what lets a key be
// introduced to a running installation.
//
// A nil Cipher passes plaintext through but rejects ciphertext, so a
// misconfigured key can never cause an unreadable value to be replayed upstream
// as if it were a credential.
func (c *Cipher) Decrypt(stored string) (string, error) {
	if !IsEncrypted(stored) {
		return stored, nil
	}
	if c == nil {
		return "", ErrNoCipher
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, envelopePrefix))
	if err != nil {
		return "", fmt.Errorf("decode encrypted value: %w", err)
	}
	nonceSize := c.aead.NonceSize()
	if len(raw) < nonceSize {
		return "", errors.New("encrypted value is too short to contain a nonce")
	}

	plaintext, err := c.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		// Deliberately vague: the common causes are the wrong key and a
		// tampered value, and distinguishing them for the caller would say more
		// about the key than the caller needs to know.
		return "", errors.New("cannot decrypt value: wrong encryption key, or the stored value is corrupt")
	}
	return string(plaintext), nil
}
