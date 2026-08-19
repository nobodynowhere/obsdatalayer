package certutil_test

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"obsdatalayer/internal/certutil"
)

func TestGenerateSelfSignedWritesCertificateAndKey(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "obsgateway.crt")
	keyFile := filepath.Join(dir, "obsgateway.key")

	err := certutil.GenerateSelfSigned(certutil.SelfSignedRequest{
		CertFile: certFile,
		KeyFile:  keyFile,
		Hosts:    []string{"localhost", "127.0.0.1"},
		ValidFor: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("generate self-signed: %v", err)
	}

	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("expected certificate PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	if err := cert.VerifyHostname("localhost"); err != nil {
		t.Errorf("expected localhost SAN: %v", err)
	}
	if err := cert.VerifyHostname("127.0.0.1"); err != nil {
		t.Errorf("expected IP SAN: %v", err)
	}
	if !cert.IsCA {
		t.Error("expected self-signed certificate to be usable as a local trust anchor")
	}

	keyInfo, err := os.Stat(keyFile)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	// Skip permission check on Windows since it doesn't support Unix-style permissions
	if runtime.GOOS != "windows" {
		if keyInfo.Mode().Perm() != 0o600 {
			t.Errorf("expected private key mode 0600, got %o", keyInfo.Mode().Perm())
		}
	}
}

func TestGenerateSelfSignedRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "obsgateway.crt")
	keyFile := filepath.Join(dir, "obsgateway.key")
	if err := os.WriteFile(certFile, []byte("existing"), 0o644); err != nil {
		t.Fatalf("write existing cert: %v", err)
	}

	err := certutil.GenerateSelfSigned(certutil.SelfSignedRequest{
		CertFile: certFile,
		KeyFile:  keyFile,
		Hosts:    []string{"localhost"},
		ValidFor: 24 * time.Hour,
	})
	if err == nil {
		t.Fatal("expected overwrite refusal")
	}
}
