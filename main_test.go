package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"obsdatalayer/internal/config"
)

func TestGenerateSelfSignedCertificateRequiresConfiguredPaths(t *testing.T) {
	err := generateSelfSignedCertificate(config.TLSConfig{}, "localhost", 365, false)
	if err == nil || !strings.Contains(err.Error(), "gateway.tls.cert_file") {
		t.Fatalf("expected missing path error, got %v", err)
	}
}

func TestGenerateSelfSignedCertificateWritesConfiguredFiles(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "gateway.crt")
	keyFile := filepath.Join(dir, "gateway.key")

	err := generateSelfSignedCertificate(config.TLSConfig{
		CertFile: certFile,
		KeyFile:  keyFile,
	}, "localhost,127.0.0.1", 30, false)
	if err != nil {
		t.Fatalf("generate certificate: %v", err)
	}
	if _, err := os.Stat(certFile); err != nil {
		t.Fatalf("expected cert file: %v", err)
	}
	if _, err := os.Stat(keyFile); err != nil {
		t.Fatalf("expected key file: %v", err)
	}
}
