package secret_test

import (
	"errors"
	"strings"
	"testing"

	"obsdatalayer/internal/secret"
)

func newTestCipher(t *testing.T) *secret.Cipher {
	t.Helper()
	encoded, err := secret.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	key, err := secret.ParseKey(encoded)
	if err != nil {
		t.Fatalf("parse key: %v", err)
	}
	c, err := secret.New(key)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	return c
}

func TestRoundTrip(t *testing.T) {
	c := newTestCipher(t)
	const plaintext = "scrape-user:s3cr3t-p@ssw0rd"

	sealed, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !secret.IsEncrypted(sealed) {
		t.Fatalf("sealed value lacks the envelope prefix: %q", sealed)
	}
	if strings.Contains(sealed, "s3cr3t") {
		t.Fatalf("plaintext leaked into the sealed value: %q", sealed)
	}

	opened, err := c.Decrypt(sealed)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if opened != plaintext {
		t.Errorf("round trip changed the value: got %q, want %q", opened, plaintext)
	}
}

// A fresh nonce per call means the same credential stored twice does not
// produce the same ciphertext, so the database cannot be mined for instances
// that share an upstream credential.
func TestEncryptIsNonDeterministic(t *testing.T) {
	c := newTestCipher(t)

	first, err := c.Encrypt("user:pass")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	second, err := c.Encrypt("user:pass")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if first == second {
		t.Error("encrypting the same value twice produced identical ciphertext")
	}
}

func TestDecryptWithWrongKeyFails(t *testing.T) {
	sealed, err := newTestCipher(t).Encrypt("user:pass")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	if _, err := newTestCipher(t).Decrypt(sealed); err == nil {
		t.Fatal("expected decryption with a different key to fail")
	}
}

// GCM authenticates the ciphertext, so a value edited in the database is
// rejected rather than decrypted to garbage and replayed upstream.
func TestDecryptRejectsTamperedValue(t *testing.T) {
	c := newTestCipher(t)
	sealed, err := c.Encrypt("user:pass")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	tampered := sealed[:len(sealed)-2] + flipLast(sealed)
	if tampered == sealed {
		t.Fatal("failed to construct a tampered value")
	}
	if _, err := c.Decrypt(tampered); err == nil {
		t.Fatal("expected a tampered value to be rejected")
	}
}

func flipLast(s string) string {
	last := s[len(s)-1]
	if last == 'A' {
		return "B="
	}
	return "A="
}

// An unset credential is not a secret, and encrypting it would hide the fact
// that the column is empty.
func TestEncryptLeavesEmptyValueEmpty(t *testing.T) {
	sealed, err := newTestCipher(t).Encrypt("")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if sealed != "" {
		t.Errorf("expected empty output for empty input, got %q", sealed)
	}
}

// Regression: Encrypt used to return any value beginning with the envelope
// prefix unchanged, to make itself idempotent. That let untrusted input decide
// whether it was encrypted -- a caller submitting the literal credential
// "enc:v1:hunter2" had it written to storage in plaintext with a key
// configured. Every non-empty value must now be sealed.
func TestEncryptSealsValuesThatLookLikeCiphertext(t *testing.T) {
	c := newTestCipher(t)
	const plaintext = envelopeLookalike

	sealed, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if sealed == plaintext {
		t.Fatal("a value beginning with the envelope prefix was stored verbatim")
	}
	if strings.Contains(sealed, "hunter2") {
		t.Fatalf("plaintext survived into the sealed value: %q", sealed)
	}

	// And it must round trip intact: the credential really is that string.
	opened, err := c.Decrypt(sealed)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if opened != plaintext {
		t.Errorf("round trip changed the value: got %q, want %q", opened, plaintext)
	}
}

// envelopeLookalike is a legitimate credential whose text collides with the
// storage marker. Named so the collision is obvious at each use.
const envelopeLookalike = "enc:v1:hunter2"

// Decrypt must not recurse: the sealed value's plaintext beginning with the
// prefix is a credential, not another envelope.
func TestDecryptDoesNotRecurseIntoPlaintext(t *testing.T) {
	c := newTestCipher(t)
	sealed, err := c.Encrypt(envelopeLookalike)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	opened, err := c.Decrypt(sealed)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if opened != envelopeLookalike {
		t.Errorf("got %q, want the credential unchanged", opened)
	}
}

// Legacy plaintext must survive a read, which is what allows a key to be
// introduced to a database that already holds credentials.
func TestDecryptPassesThroughPlaintext(t *testing.T) {
	got, err := newTestCipher(t).Decrypt("user:pass")
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != "user:pass" {
		t.Errorf("plaintext was altered: got %q", got)
	}
}

// The nil cipher is how "encryption not configured" is represented. It must not
// panic, and above all it must never hand ciphertext to a caller that would
// replay it upstream as a credential.
func TestNilCipher(t *testing.T) {
	var c *secret.Cipher

	got, err := c.Encrypt("user:pass")
	if err != nil {
		t.Fatalf("nil encrypt: %v", err)
	}
	if got != "user:pass" {
		t.Errorf("nil cipher altered plaintext on encrypt: %q", got)
	}

	got, err = c.Decrypt("user:pass")
	if err != nil {
		t.Fatalf("nil decrypt: %v", err)
	}
	if got != "user:pass" {
		t.Errorf("nil cipher altered plaintext on decrypt: %q", got)
	}

	sealed, err := newTestCipher(t).Encrypt("user:pass")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := c.Decrypt(sealed); !errors.Is(err, secret.ErrNoCipher) {
		t.Errorf("expected ErrNoCipher for ciphertext with no key, got %v", err)
	}
}

func TestNewRejectsWrongKeySize(t *testing.T) {
	for _, size := range []int{0, 16, 31, 33, 64} {
		if _, err := secret.New(make([]byte, size)); err == nil {
			t.Errorf("expected a %d-byte key to be rejected", size)
		}
	}
}

func TestParseKey(t *testing.T) {
	valid, err := secret.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	// Key files are usually made with a shell redirect, which leaves a newline.
	if _, err := secret.ParseKey("  " + valid + "\n"); err != nil {
		t.Errorf("expected surrounding whitespace to be tolerated: %v", err)
	}

	for name, input := range map[string]string{
		"empty":        "",
		"whitespace":   "   \n",
		"not base64":   "this is not base64!!",
		"wrong length": "c2hvcnQ=",
	} {
		if _, err := secret.ParseKey(input); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
