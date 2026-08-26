package secret_test

import (
	"os"
	"path/filepath"
	"testing"

	"obsdatalayer/internal/secret"
)

// No key configured is not an error here: whether the gateway may run without
// one depends on what is in the database, which this package cannot see.
func TestLoadKeyAbsent(t *testing.T) {
	t.Setenv(secret.EnvKey, "")

	key, source, err := secret.LoadKey("")
	if err != nil {
		t.Fatalf("load key: %v", err)
	}
	if key != nil {
		t.Errorf("expected no key, got %d bytes", len(key))
	}
	if source != "" {
		t.Errorf("expected no source, got %q", source)
	}
}

func TestLoadKeyFromEnv(t *testing.T) {
	encoded, err := secret.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	t.Setenv(secret.EnvKey, encoded)

	key, source, err := secret.LoadKey("")
	if err != nil {
		t.Fatalf("load key: %v", err)
	}
	if len(key) != secret.KeySize {
		t.Fatalf("expected a %d-byte key, got %d", secret.KeySize, len(key))
	}
	if source != "environment "+secret.EnvKey {
		t.Errorf("unexpected source %q", source)
	}
}

func TestLoadKeyMissingFile(t *testing.T) {
	t.Setenv(secret.EnvKey, "")

	if _, _, err := secret.LoadKey(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("expected an error for a missing key file")
	}
}

func TestLoadKeyRejectsMalformedFile(t *testing.T) {
	t.Setenv(secret.EnvKey, "")
	// Use a simple file write without permission checks for this cross-platform test
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	if _, _, err := secret.LoadKey(path); err == nil {
		t.Fatal("expected an error for a malformed key file")
	}
}
