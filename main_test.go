package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/db"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/proxy"
)

func TestSelfSignedFilesUsesExplicitDirectory(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, err := selfSignedFiles(dir)
	if err != nil {
		t.Fatalf("resolve files: %v", err)
	}
	if certFile != filepath.Join(dir, "obsgateway.crt") {
		t.Errorf("unexpected cert path %q", certFile)
	}
	if keyFile != filepath.Join(dir, "obsgateway.key") {
		t.Errorf("unexpected key path %q", keyFile)
	}
}

func TestSelfSignedFilesRejectsEmptyDirectory(t *testing.T) {
	_, _, err := selfSignedFiles("")
	if err == nil {
		t.Fatal("expected empty directory to fail")
	}
}

func TestGenerateSelfSignedCertificateWritesGeneratedFiles(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "gateway.crt")
	keyFile := filepath.Join(dir, "gateway.key")

	err := generateSelfSignedCertificate(selfSignedOptions{
		ConfigPath: "gateway.yaml",
		Bootstrap:  &config.Bootstrap{DB: db.Config{Type: "sqlite", Path: ":memory:"}},
		Hosts:      "localhost,127.0.0.1",
		Days:       30,
		Dir:        dir,
	})
	if err != nil {
		t.Fatalf("generate certificate: %v", err)
	}
	certFile = filepath.Join(dir, "obsgateway.crt")
	keyFile = filepath.Join(dir, "obsgateway.key")
	if _, err := os.Stat(certFile); err != nil {
		t.Fatalf("expected cert file: %v", err)
	}
	if _, err := os.Stat(keyFile); err != nil {
		t.Fatalf("expected key file: %v", err)
	}
}

func TestGenerateSelfSignedCertificateCanUpdateBootstrapConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "gateway.yaml")
	certDir := filepath.Join(dir, "tls")
	initial := []byte("db:\n  type: sqlite\n  path: ./gateway.db\ngateway:\n  listen: 8080\n  admin_listen: 9091\n  tls:\n    enabled: false\n")
	if err := os.WriteFile(configPath, initial, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	bootstrap, err := config.LoadBootstrapForTLSGeneration(configPath)
	if err != nil {
		t.Fatalf("load bootstrap: %v", err)
	}

	err = generateSelfSignedCertificate(selfSignedOptions{
		ConfigPath:   configPath,
		Bootstrap:    bootstrap,
		Hosts:        "localhost",
		Days:         30,
		Dir:          certDir,
		UpdateConfig: true,
	})
	if err != nil {
		t.Fatalf("generate certificate: %v", err)
	}

	updated, err := config.LoadBootstrap(configPath)
	if err != nil {
		t.Fatalf("load updated bootstrap: %v", err)
	}
	if !updated.Gateway.TLS.Enabled {
		t.Fatal("expected TLS to be enabled")
	}
	if updated.Gateway.TLS.CertFile != filepath.Join(certDir, "obsgateway.crt") {
		t.Errorf("unexpected cert path %q", updated.Gateway.TLS.CertFile)
	}
	if updated.Gateway.TLS.KeyFile != filepath.Join(certDir, "obsgateway.key") {
		t.Errorf("unexpected key path %q", updated.Gateway.TLS.KeyFile)
	}
}

// TestReadyIsAnsweredByTheGateway pins that readiness reports on the gateway
// itself rather than being proxied to Mimir. A probe answered by a backend
// would report the gateway unready whenever that backend was down, and ready
// whenever it was up -- the opposite of what a readiness probe is for.
func TestReadyIsAnsweredByTheGateway(t *testing.T) {
	cfg, err := config.New(&config.Config{Instances: []*config.InstanceConfig{}})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	holder := config.NewHolder(cfg, "")

	// No upstream client at all: a proxied /ready could not succeed here.
	handler := dataHandler(holder, nil, proxy.New(nil, nil), metrics.New(prometheus.NewRegistry()), nil)

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ready" {
		t.Errorf("expected status ready, got %v", body["status"])
	}
	// A fresh install with nothing configured is still ready, and says so.
	if body["instances"] != float64(0) {
		t.Errorf("expected instances 0, got %v", body["instances"])
	}
}

// TestReadyNeedsNoCredentials keeps container probes working; both /ready and
// /healthz are terminated by the gateway and never forwarded.
func TestReadyNeedsNoCredentials(t *testing.T) {
	cfg, err := config.New(&config.Config{Instances: []*config.InstanceConfig{}})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	handler := dataHandler(config.NewHolder(cfg, ""), nil, proxy.New(nil, nil), metrics.New(prometheus.NewRegistry()), nil)

	for _, path := range []string{"/ready", "/healthz"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200 without credentials, got %d", rec.Code)
			}
		})
	}
}

// withStdin points os.Stdin at a pipe carrying in. A pipe is not a terminal, so
// this also selects the non-interactive branch of readPasswordTwice.
func withStdin(t *testing.T, in string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	go func() {
		defer func() { _ = w.Close() }()
		_, _ = w.WriteString(in)
	}()

	original := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = original
		_ = r.Close()
	})
}

func TestPromptNewPasswordAcceptsMatchingEntries(t *testing.T) {
	withStdin(t, "correcthorsebattery\ncorrecthorsebattery\n")

	got, err := promptNewPassword("admin")
	if err != nil {
		t.Fatalf("promptNewPassword: %v", err)
	}
	if got != "correcthorsebattery" {
		t.Errorf("got %q, want %q", got, "correcthorsebattery")
	}
}

// A typo during a lockout recovery would lock the operator out a second time,
// so a mismatch has to fail rather than silently take the first entry.
func TestPromptNewPasswordRejectsMismatch(t *testing.T) {
	withStdin(t, "correcthorsebattery\ncorrecthorsebatterz\n")

	if _, err := promptNewPassword("admin"); err == nil {
		t.Fatal("expected mismatched entries to be refused")
	}
}

func TestPromptNewPasswordNeedsBothEntries(t *testing.T) {
	withStdin(t, "correcthorsebattery\n")

	if _, err := promptNewPassword("admin"); err == nil {
		t.Fatal("expected a single entry to be refused")
	}
}

// The confirmation is a comparison, not a normalisation: a trailing space is
// part of the password, and two entries that differ only there do not match.
func TestPromptNewPasswordDoesNotTrimEntries(t *testing.T) {
	withStdin(t, "correcthorsebattery \ncorrecthorsebattery\n")

	if _, err := promptNewPassword("admin"); err == nil {
		t.Fatal("expected entries differing by whitespace to be refused")
	}
}

// logWithCallerRenaming formats one record through the handler configuration
// setupLogging installs, and returns the line.
func logWithCallerRenaming(t *testing.T, log func(l *slog.Logger)) string {
	t.Helper()
	var buf bytes.Buffer
	log(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		AddSource:   true,
		ReplaceAttr: renameSourceToCaller,
	})))
	return buf.String()
}

func TestSetupLoggingRecordsTheCallSite(t *testing.T) {
	got := logWithCallerRenaming(t, func(l *slog.Logger) { l.Info("listener started") })

	if !strings.Contains(got, "caller=") {
		t.Fatalf("expected a caller field, got %s", got)
	}
	if !strings.Contains(got, "main_test.go:") {
		t.Errorf("expected the caller to name this file, got %s", got)
	}
	if strings.Contains(got, "source=") {
		t.Errorf("built-in source key was not renamed, got %s", got)
	}
}

// Two lines carry a top-level attribute of their own called "source" -- the
// config path and the throttled client. Renaming the built-in must not rename
// those, or an operator alerting on them silently stops matching.
func TestSetupLoggingLeavesCallerAttributesNamedSourceAlone(t *testing.T) {
	got := logWithCallerRenaming(t, func(l *slog.Logger) {
		l.Info("config reloaded", "instances", 3, "source", "./gateway.yaml")
	})

	if !strings.Contains(got, "source=./gateway.yaml") {
		t.Errorf("the line's own source attribute was rewritten, got %s", got)
	}
	if !strings.Contains(got, "caller=") {
		t.Errorf("expected a caller field, got %s", got)
	}
	if strings.Count(got, "caller=") != 1 {
		t.Errorf("expected exactly one caller field, got %s", got)
	}
}
