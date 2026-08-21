package auth

import (
	"strings"
	"testing"
)

// Regression: the secret half is base64url, whose alphabet includes "_". An
// unlimited split on "_" tore such a token into four pieces and rejected it, so
// roughly half of all issued keys were silently unusable. The failure looked
// intermittent because it depended on the random secret.
func TestParseAPIKeyHandlesUnderscoresInTheSecret(t *testing.T) {
	const handle = "0123456789ab"
	const secret = "abc_def_ghi-jkl"

	gotHandle, gotSecret, err := parseAPIKey(keyPrefixLabel + "_" + handle + "_" + secret)
	if err != nil {
		t.Fatalf("a secret containing underscores was rejected: %v", err)
	}
	if gotHandle != handle {
		t.Errorf("handle = %q, want %q", gotHandle, handle)
	}
	if gotSecret != secret {
		t.Errorf("secret = %q, want %q -- the split must not consume its underscores", gotSecret, secret)
	}
}

// Every generated key must parse and verify. Run enough times that a token
// containing "_" is a near certainty rather than a coin flip.
func TestGeneratedKeysAlwaysRoundTrip(t *testing.T) {
	withUnderscore := 0
	for i := 0; i < 200; i++ {
		secret, handle, hash, err := generateAPIKey()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		gotHandle, rawSecret, err := parseAPIKey(secret)
		if err != nil {
			t.Fatalf("generated key %q does not parse: %v", secret, err)
		}
		if gotHandle != handle {
			t.Fatalf("handle round trip: got %q want %q", gotHandle, handle)
		}
		if !verifyAPIKeySecret(rawSecret, hash) {
			t.Fatalf("generated key %q does not verify against its own hash", secret)
		}
		// Count how many carry the character that broke the old parser.
		if strings.Contains(strings.SplitN(secret, "_", 3)[2], "_") {
			withUnderscore++
		}
	}
	if withUnderscore == 0 {
		t.Skip("no generated secret contained an underscore; the regression was not exercised")
	}
	t.Logf("%d of 200 generated secrets contained an underscore", withUnderscore)
}

func TestVerifyAPIKeySecretRejectsWrongSecret(t *testing.T) {
	_, _, hash, err := generateAPIKey()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if verifyAPIKeySecret("not-the-secret", hash) {
		t.Error("a wrong secret verified")
	}
}
