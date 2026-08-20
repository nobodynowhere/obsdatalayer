package config

import (
	"fmt"
	"log/slog"

	"gorm.io/gorm"

	dbstore "obsdatalayer/internal/db"
	"obsdatalayer/internal/secret"
)

// credentialRef is one stored credential, identified well enough to update it
// in place and to name it in an error.
type credentialRef struct {
	table string
	id    any
	label string
	value string
}

// EnsureCredentialsEncrypted reconciles the stored upstream credentials with the
// configured encryption key. It runs once at startup, before the config is
// loaded, and is the single place that decides whether the process may run.
//
// The rules:
//
//   - No key and nothing stored: fine. A fresh install has no secret to protect
//     and should not be made to invent a key before it can start.
//   - No key but credentials are stored: refuse. Starting would silently leave
//     them in plaintext, which is the defect this exists to close.
//   - A key, and plaintext credentials: encrypt them in place and report how
//     many. This is the upgrade path.
//   - A key that cannot open an already-encrypted value: refuse. Serving on
//     would replay unusable credentials upstream and produce 401s that look
//     like an upstream problem rather than a key problem.
func EnsureCredentialsEncrypted(db *gorm.DB, c *secret.Cipher) error {
	refs, err := loadCredentialRefs(db)
	if err != nil {
		return err
	}

	var plaintext, encrypted []credentialRef
	for _, ref := range refs {
		if secret.IsEncrypted(ref.value) {
			encrypted = append(encrypted, ref)
			continue
		}
		plaintext = append(plaintext, ref)
	}

	if c == nil {
		if len(encrypted) > 0 {
			return fmt.Errorf(
				"%d stored upstream credential(s) are encrypted but no encryption key is configured; "+
					"set %s or gateway.encryption_key_file in the bootstrap config",
				len(encrypted), secret.EnvKey)
		}
		if len(plaintext) > 0 {
			return fmt.Errorf(
				"%d upstream credential(s) are stored in plaintext and no encryption key is configured; "+
					"generate one with --generate-encryption-key, then set %s or "+
					"gateway.encryption_key_file in the bootstrap config",
				len(plaintext), secret.EnvKey)
		}
		return nil
	}

	// Verify the key against what is already sealed before writing anything. A
	// wrong key must not be allowed to encrypt the remaining plaintext with a
	// key that cannot read the rest.
	for _, ref := range encrypted {
		if _, err := c.Decrypt(ref.value); err != nil {
			return fmt.Errorf("%s: %w", ref.label, err)
		}
	}

	if len(plaintext) == 0 {
		slog.Debug("credential encryption verified", "encrypted", len(encrypted))
		return nil
	}

	if err := encryptRefs(db, c, plaintext); err != nil {
		return err
	}
	slog.Warn("encrypted stored upstream credentials at rest",
		"migrated", len(plaintext), "already_encrypted", len(encrypted))
	return nil
}

// loadCredentialRefs collects every non-empty stored credential. Only the
// columns needed are selected: the rest of the row is irrelevant here and
// loading it would pull unrelated secrets into memory for no reason.
func loadCredentialRefs(db *gorm.DB) ([]credentialRef, error) {
	var refs []credentialRef

	var instances []dbstore.Instance
	if err := db.Select("id", "name", "basic_auth").Find(&instances).Error; err != nil {
		return nil, fmt.Errorf("load instance credentials: %w", err)
	}
	for _, inst := range instances {
		if inst.BasicAuth == "" {
			continue
		}
		refs = append(refs, credentialRef{
			table: "instances",
			id:    inst.ID,
			label: fmt.Sprintf("instance %q basic_auth", inst.Name),
			value: inst.BasicAuth,
		})
	}

	var targets []dbstore.PushTarget
	if err := db.Select("id", "url", "basic_auth").Find(&targets).Error; err != nil {
		return nil, fmt.Errorf("load push target credentials: %w", err)
	}
	for _, pt := range targets {
		if pt.BasicAuth == "" {
			continue
		}
		refs = append(refs, credentialRef{
			table: "push_targets",
			id:    pt.ID,
			label: fmt.Sprintf("push target %q basic_auth", pt.URL),
			value: pt.BasicAuth,
		})
	}

	return refs, nil
}

// encryptRefs rewrites the named credentials as ciphertext in one transaction,
// so an interrupted migration cannot leave the table half converted.
func encryptRefs(db *gorm.DB, c *secret.Cipher, refs []credentialRef) error {
	return db.Transaction(func(tx *gorm.DB) error {
		for _, ref := range refs {
			sealed, err := c.Encrypt(ref.value)
			if err != nil {
				return fmt.Errorf("encrypt %s: %w", ref.label, err)
			}
			res := tx.Table(ref.table).Where("id = ?", ref.id).Update("basic_auth", sealed)
			if res.Error != nil {
				return fmt.Errorf("store encrypted %s: %w", ref.label, res.Error)
			}
			if res.RowsAffected != 1 {
				return fmt.Errorf("store encrypted %s: expected 1 row updated, got %d", ref.label, res.RowsAffected)
			}
		}
		return nil
	})
}
