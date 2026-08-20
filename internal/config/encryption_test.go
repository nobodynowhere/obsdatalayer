package config_test

import (
	"strings"
	"testing"

	"gorm.io/gorm"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/secret"
)

func testCipher(t *testing.T) *secret.Cipher {
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

// storedAuth reads the basic_auth column directly, bypassing the mapping layer,
// so a test can assert on what is actually on disk rather than on what the
// loader chose to hand back.
func storedAuth(t *testing.T, db *gorm.DB, table, where string, arg any) string {
	t.Helper()
	var got string
	if err := db.Table(table).Where(where, arg).Pluck("basic_auth", &got).Error; err != nil {
		t.Fatalf("read %s.basic_auth: %v", table, err)
	}
	return got
}

func authInstance(name string) *config.InstanceConfig {
	return &config.InstanceConfig{
		Name:      name,
		Backend:   "loki",
		URL:       "http://loki.local",
		BasicAuth: "scrape:s3cr3t",
	}
}

// The point of the whole exercise: what lands in the database is ciphertext,
// and what the gateway holds in memory is the plaintext it must replay upstream.
func TestCredentialsAreEncryptedAtRest(t *testing.T) {
	gormDB := openTestConfigDB(t)
	c := testCipher(t)

	inst := authInstance("loki-prod")
	inst.PushURLs = nil
	if err := config.CreateInstance(gormDB, inst, nil, c); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	stored := storedAuth(t, gormDB, "instances", "name = ?", "loki-prod")
	if !secret.IsEncrypted(stored) {
		t.Fatalf("stored credential is not encrypted: %q", stored)
	}
	if strings.Contains(stored, "s3cr3t") {
		t.Fatalf("plaintext credential is readable in the database: %q", stored)
	}

	cfg, err := config.LoadFromDB(gormDB, c)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Instances[0].BasicAuth; got != "scrape:s3cr3t" {
		t.Errorf("loaded credential = %q, want the decrypted plaintext", got)
	}
}

func TestPushTargetCredentialsAreEncryptedAtRest(t *testing.T) {
	gormDB := openTestConfigDB(t)
	c := testCipher(t)

	inst := &config.InstanceConfig{
		Name:    "mimir-fanout",
		Backend: "mimir",
		PushURLs: []config.PushTarget{
			{URL: "http://a.local", BasicAuth: "a-user:a-pass"},
			{URL: "http://b.local", BasicAuth: "b-user:b-pass"},
		},
	}
	if err := config.CreateInstance(gormDB, inst, nil, c); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	for _, url := range []string{"http://a.local", "http://b.local"} {
		stored := storedAuth(t, gormDB, "push_targets", "url = ?", url)
		if !secret.IsEncrypted(stored) {
			t.Errorf("%s: stored credential is not encrypted: %q", url, stored)
		}
	}
	if stored := storedAuth(t, gormDB, "push_targets", "url = ?", "http://a.local"); strings.Contains(stored, "a-pass") {
		t.Errorf("plaintext leaked into the database: %q", stored)
	}

	cfg, err := config.LoadFromDB(gormDB, c)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	targets := cfg.Instances[0].PushURLs
	if len(targets) != 2 {
		t.Fatalf("expected 2 push targets, got %d", len(targets))
	}
	// Each target must come back with its own credential. Getting these crossed
	// would send one upstream's password to another.
	if targets[0].BasicAuth != "a-user:a-pass" || targets[1].BasicAuth != "b-user:b-pass" {
		t.Errorf("credentials did not round trip per target: %q, %q",
			targets[0].BasicAuth, targets[1].BasicAuth)
	}
}

// The upgrade path: a database written before encryption existed is converted
// in place the first time a key is configured.
func TestEnsureCredentialsEncryptedMigratesPlaintext(t *testing.T) {
	gormDB := openTestConfigDB(t)

	// Written with no cipher, exactly as an older build would have stored it.
	if err := config.CreateInstance(gormDB, authInstance("loki-legacy"), nil, nil); err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if stored := storedAuth(t, gormDB, "instances", "name = ?", "loki-legacy"); stored != "scrape:s3cr3t" {
		t.Fatalf("expected plaintext to start with, got %q", stored)
	}

	c := testCipher(t)
	if err := config.EnsureCredentialsEncrypted(gormDB, c); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	stored := storedAuth(t, gormDB, "instances", "name = ?", "loki-legacy")
	if !secret.IsEncrypted(stored) {
		t.Fatalf("credential was not migrated: %q", stored)
	}

	cfg, err := config.LoadFromDB(gormDB, c)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Instances[0].BasicAuth; got != "scrape:s3cr3t" {
		t.Errorf("migrated credential = %q, want the original plaintext", got)
	}
}

// Restarting must not re-wrap what is already sealed.
func TestEnsureCredentialsEncryptedIsIdempotent(t *testing.T) {
	gormDB := openTestConfigDB(t)
	c := testCipher(t)

	if err := config.CreateInstance(gormDB, authInstance("loki-prod"), nil, c); err != nil {
		t.Fatalf("create instance: %v", err)
	}
	first := storedAuth(t, gormDB, "instances", "name = ?", "loki-prod")

	for i := 0; i < 3; i++ {
		if err := config.EnsureCredentialsEncrypted(gormDB, c); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if got := storedAuth(t, gormDB, "instances", "name = ?", "loki-prod"); got != first {
		t.Error("a no-op run rewrote the stored credential")
	}
}

// The operator's decision, enforced: if there is a credential to protect and no
// key to protect it with, the process does not start.
func TestEnsureCredentialsEncryptedRefusesPlaintextWithoutKey(t *testing.T) {
	gormDB := openTestConfigDB(t)

	if err := config.CreateInstance(gormDB, authInstance("loki-prod"), nil, nil); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	err := config.EnsureCredentialsEncrypted(gormDB, nil)
	if err == nil {
		t.Fatal("expected a refusal to start with unencrypted credentials")
	}
	if !strings.Contains(err.Error(), "--generate-encryption-key") {
		t.Errorf("error should tell the operator how to fix it, got: %v", err)
	}
}

// A fresh install has nothing to protect, and should not have to invent a key
// before it can start.
func TestEnsureCredentialsEncryptedAllowsEmptyDatabaseWithoutKey(t *testing.T) {
	gormDB := openTestConfigDB(t)

	if err := config.EnsureCredentialsEncrypted(gormDB, nil); err != nil {
		t.Fatalf("expected an empty database to start without a key: %v", err)
	}
}

// An instance with no credential at all is not something to protect either.
func TestEnsureCredentialsEncryptedIgnoresEmptyCredentials(t *testing.T) {
	gormDB := openTestConfigDB(t)

	inst := authInstance("loki-open")
	inst.BasicAuth = ""
	if err := config.CreateInstance(gormDB, inst, nil, nil); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	if err := config.EnsureCredentialsEncrypted(gormDB, nil); err != nil {
		t.Fatalf("an unset credential should not require a key: %v", err)
	}
}

// Losing the key must be a loud startup failure, not 401s from upstream that
// look like someone rotated a password.
func TestEnsureCredentialsEncryptedRefusesCiphertextWithoutKey(t *testing.T) {
	gormDB := openTestConfigDB(t)

	if err := config.CreateInstance(gormDB, authInstance("loki-prod"), nil, testCipher(t)); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	if err := config.EnsureCredentialsEncrypted(gormDB, nil); err == nil {
		t.Fatal("expected a refusal to start with ciphertext and no key")
	}
}

func TestEnsureCredentialsEncryptedRefusesWrongKey(t *testing.T) {
	gormDB := openTestConfigDB(t)

	if err := config.CreateInstance(gormDB, authInstance("loki-prod"), nil, testCipher(t)); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	err := config.EnsureCredentialsEncrypted(gormDB, testCipher(t))
	if err == nil {
		t.Fatal("expected a refusal to start with the wrong key")
	}
	if !strings.Contains(err.Error(), "loki-prod") {
		t.Errorf("error should name the offending credential, got: %v", err)
	}
}

// A wrong key must not convert the remaining plaintext with a key that cannot
// read what is already sealed, which would split the table across two keys.
func TestWrongKeyDoesNotMigrateRemainingPlaintext(t *testing.T) {
	gormDB := openTestConfigDB(t)

	if err := config.CreateInstance(gormDB, authInstance("loki-sealed"), nil, testCipher(t)); err != nil {
		t.Fatalf("create sealed instance: %v", err)
	}
	plain := authInstance("loki-plain")
	if err := config.CreateInstance(gormDB, plain, nil, nil); err != nil {
		t.Fatalf("create plaintext instance: %v", err)
	}

	if err := config.EnsureCredentialsEncrypted(gormDB, testCipher(t)); err == nil {
		t.Fatal("expected the wrong key to be rejected")
	}
	if got := storedAuth(t, gormDB, "instances", "name = ?", "loki-plain"); got != "scrape:s3cr3t" {
		t.Errorf("plaintext was migrated despite the key being rejected: %q", got)
	}
}

// A load with no key must fail rather than hand ciphertext to the proxy, which
// would replay the envelope string upstream as if it were a password.
func TestLoadFromDBRefusesCiphertextWithoutKey(t *testing.T) {
	gormDB := openTestConfigDB(t)

	if err := config.CreateInstance(gormDB, authInstance("loki-prod"), nil, testCipher(t)); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	if _, err := config.LoadFromDB(gormDB, nil); err == nil {
		t.Fatal("expected loading ciphertext without a key to fail")
	}
}

// Editing an instance re-seals its credential rather than writing back the
// plaintext the API layer handed over.
func TestUpdateInstanceKeepsCredentialEncrypted(t *testing.T) {
	gormDB := openTestConfigDB(t)
	c := testCipher(t)

	if err := config.CreateInstance(gormDB, authInstance("loki-prod"), nil, c); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	updated := authInstance("loki-prod")
	updated.BasicAuth = "scrape:rotated-password"
	if err := config.UpdateInstance(gormDB, "loki-prod", updated, nil, c); err != nil {
		t.Fatalf("update instance: %v", err)
	}

	stored := storedAuth(t, gormDB, "instances", "name = ?", "loki-prod")
	if !secret.IsEncrypted(stored) {
		t.Fatalf("updated credential is not encrypted: %q", stored)
	}
	if strings.Contains(stored, "rotated-password") {
		t.Fatalf("plaintext leaked into the database: %q", stored)
	}

	cfg, err := config.LoadFromDB(gormDB, c)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Instances[0].BasicAuth; got != "scrape:rotated-password" {
		t.Errorf("updated credential = %q, want the rotated plaintext", got)
	}
}

// Regression: a credential whose text happens to begin with the storage marker
// was written to the database verbatim, in plaintext, with a key configured.
// The marker describes what the gateway wrote; it is never evidence about a
// value submitted from outside.
func TestCredentialBeginningWithEnvelopePrefixIsEncrypted(t *testing.T) {
	gormDB := openTestConfigDB(t)
	c := testCipher(t)

	const credential = "enc:v1:my-real-password"
	inst := authInstance("loki-lookalike")
	inst.BasicAuth = credential
	if err := config.CreateInstance(gormDB, inst, nil, c); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	stored := storedAuth(t, gormDB, "instances", "name = ?", "loki-lookalike")
	if stored == credential {
		t.Fatal("the credential was stored verbatim")
	}
	if strings.Contains(stored, "my-real-password") {
		t.Fatalf("plaintext is readable in the database: %q", stored)
	}

	// It must still be the credential the operator typed.
	cfg, err := config.LoadFromDB(gormDB, c)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Instances[0].BasicAuth; got != credential {
		t.Errorf("credential = %q, want %q", got, credential)
	}
}

func TestPushTargetCredentialBeginningWithEnvelopePrefixIsEncrypted(t *testing.T) {
	gormDB := openTestConfigDB(t)
	c := testCipher(t)

	const credential = "enc:v1:target-password"
	inst := &config.InstanceConfig{
		Name:     "mimir-lookalike",
		Backend:  "mimir",
		PushURLs: []config.PushTarget{{URL: "http://a.local", BasicAuth: credential}},
	}
	if err := config.CreateInstance(gormDB, inst, nil, c); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	stored := storedAuth(t, gormDB, "push_targets", "url = ?", "http://a.local")
	if strings.Contains(stored, "target-password") {
		t.Fatalf("plaintext is readable in the database: %q", stored)
	}

	cfg, err := config.LoadFromDB(gormDB, c)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Instances[0].PushURLs[0].BasicAuth; got != credential {
		t.Errorf("credential = %q, want %q", got, credential)
	}
}

// The startup migration must still not double-wrap: it partitions on the marker
// and only hands plaintext to Encrypt, which is why removing the shortcut there
// is safe.
func TestMigrationDoesNotReEncryptSealedValues(t *testing.T) {
	gormDB := openTestConfigDB(t)
	c := testCipher(t)

	if err := config.CreateInstance(gormDB, authInstance("loki-prod"), nil, c); err != nil {
		t.Fatalf("create instance: %v", err)
	}
	sealed := storedAuth(t, gormDB, "instances", "name = ?", "loki-prod")

	for i := 0; i < 3; i++ {
		if err := config.EnsureCredentialsEncrypted(gormDB, c); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if got := storedAuth(t, gormDB, "instances", "name = ?", "loki-prod"); got != sealed {
		t.Error("an already sealed value was rewritten by the migration")
	}

	cfg, err := config.LoadFromDB(gormDB, c)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Instances[0].BasicAuth; got != "scrape:s3cr3t" {
		t.Errorf("credential = %q, want the original plaintext (double-wrapped?)", got)
	}
}
