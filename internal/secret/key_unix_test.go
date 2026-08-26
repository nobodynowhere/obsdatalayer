//go:build !windows
// +build !windows

package secret_test

import (
	"os"
	"path/filepath"
	"testing"

	"obsdatalayer/internal/secret"
)

func writeKeyFile(t *testing.T, dir, name, contents string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	// WriteFile respects the process umask, so set the mode explicitly.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod key file: %v", err)
	}
	return path
}

func TestLoadKeyFromFile(t *testing.T) {
	t.Setenv(secret.EnvKey, "")
	encoded, err := secret.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	path := writeKeyFile(t, t.TempDir(), "key", encoded+"\n", 0o600)

	key, source, err := secret.LoadKey(path)
	if err != nil {
		t.Fatalf("load key: %v", err)
	}
	if len(key) != secret.KeySize {
		t.Fatalf("expected a %d-byte key, got %d", secret.KeySize, len(key))
	}
	if source != "file "+path {
		t.Errorf("unexpected source %q", source)
	}
}

// A container injects the key by environment even when the config file it
// ships with names a path, so the environment has to win.
func TestLoadKeyEnvBeatsFile(t *testing.T) {
	envKey, err := secret.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	fileKey, err := secret.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	if envKey == fileKey {
		t.Fatal("generated identical keys")
	}
	t.Setenv(secret.EnvKey, envKey)
	path := writeKeyFile(t, t.TempDir(), "key", fileKey, 0o600)

	_, source, err := secret.LoadKey(path)
	if err != nil {
		t.Fatalf("load key: %v", err)
	}
	if source != "environment "+secret.EnvKey {
		t.Errorf("expected the environment to win, got source %q", source)
	}
}

// The key is the only thing standing between a database backup and every
// upstream credential, so a key file anyone can read defeats the purpose.
func TestLoadKeyRejectsLooseFilePermissions(t *testing.T) {
	t.Setenv(secret.EnvKey, "")
	encoded, err := secret.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o666} {
		path := writeKeyFile(t, t.TempDir(), "key", encoded, mode)
		if _, _, err := secret.LoadKey(path); err == nil {
			t.Errorf("mode %#o: expected a permissions error", mode)
		}
	}
}

func TestWriteKeyFile(t *testing.T) {
	encoded, err := secret.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "key")

	if err := secret.WriteKeyFile(path, encoded); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("expected mode 0600, got %#o", perm)
	}

	// The written file must be loadable, or --generate-encryption-key produces
	// something the gateway then refuses.
	t.Setenv(secret.EnvKey, "")
	if _, _, err := secret.LoadKey(path); err != nil {
		t.Errorf("generated key file does not load: %v", err)
	}
}

// Overwriting a key would strand every credential already encrypted with the
// old one, with no way back.
func TestWriteKeyFileRefusesToOverwrite(t *testing.T) {
	encoded, err := secret.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	path := writeKeyFile(t, t.TempDir(), "key", "existing", 0o600)

	if err := secret.WriteKeyFile(path, encoded); err == nil {
		t.Fatal("expected a refusal to overwrite an existing key file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != "existing" {
		t.Error("the existing key file was modified")
	}
}
