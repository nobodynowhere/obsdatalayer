package secret

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strings"
)

// EnvKey names the environment variable carrying the key material directly.
// It takes precedence over the bootstrap file's key path so that a container
// can inject the key without rewriting the config file it ships with.
const EnvKey = "OBSGATEWAY_ENCRYPTION_KEY"

// GenerateKey returns a new base64-encoded 32-byte key.
func GenerateKey() (string, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// ParseKey decodes a base64 key and checks its length.
//
// Surrounding whitespace is trimmed because the overwhelmingly common way to
// produce a key file is a shell redirect, which leaves a trailing newline.
func ParseKey(encoded string) ([]byte, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, fmt.Errorf("encryption key is empty")
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("encryption key is not valid base64: %w", err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("encryption key must decode to %d bytes, got %d", KeySize, len(key))
	}
	return key, nil
}

// LoadKey resolves the encryption key from the environment or from the file
// named in the bootstrap config, in that order. It reports the source so the
// startup log can say where the key came from without disclosing the key.
//
// A missing key is not an error here. Whether the gateway may run without one
// depends on what is already in the database, which this package cannot see.
func LoadKey(keyFile string) (key []byte, source string, err error) {
	if encoded := os.Getenv(EnvKey); strings.TrimSpace(encoded) != "" {
		key, err := ParseKey(encoded)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", EnvKey, err)
		}
		return key, "environment " + EnvKey, nil
	}

	keyFile = strings.TrimSpace(keyFile)
	if keyFile == "" {
		return nil, "", nil
	}

	info, err := os.Stat(keyFile)
	if err != nil {
		return nil, "", fmt.Errorf("encryption key file %s: %w", keyFile, err)
	}
	// The key is the one thing standing between a database backup and every
	// upstream credential, so a world- or group-readable key file defeats the
	// purpose. Refuse rather than warn: this is caught once, at startup, by the
	// operator who just created the file.
	// On Windows, we skip the Unix-style permission check since Windows uses ACLs.
	// The operator is responsible for setting appropriate Windows ACLs.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return nil, "", fmt.Errorf(
				"encryption key file %s is readable by group or others (mode %#o); run: chmod 600 %s",
				keyFile, perm, keyFile)
		}
	}

	data, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, "", fmt.Errorf("read encryption key file %s: %w", keyFile, err)
	}
	parsed, err := ParseKey(string(data))
	if err != nil {
		return nil, "", fmt.Errorf("encryption key file %s: %w", keyFile, err)
	}
	return parsed, "file " + keyFile, nil
}

// WriteKeyFile writes a generated key with owner-only permissions, refusing to
// clobber an existing file. Overwriting a key would strand every credential
// already encrypted with the old one.
func WriteKeyFile(path, encoded string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("encryption key file %s already exists; refusing to overwrite it", path)
		}
		return fmt.Errorf("create encryption key file %s: %w", path, err)
	}
	defer f.Close()

	if _, err := f.WriteString(encoded + "\n"); err != nil {
		return fmt.Errorf("write encryption key file %s: %w", path, err)
	}
	return nil
}
