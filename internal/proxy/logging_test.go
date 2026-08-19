package proxy

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"obsdatalayer/internal/config"
)

func captureLogs(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestLogUpstreamNon2XXIncludesBodyPreview(t *testing.T) {
	logs := captureLogs(t, slog.LevelWarn)

	LogUpstreamNon2XX("mimir-prod", http.MethodGet, "http://mimir.local/prometheus/api/v1/query", http.StatusBadRequest, time.Millisecond, "tenant-a", []byte(`{"error":"bad query"}`), nil)

	got := logs.String()
	for _, want := range []string{
		"upstream returned non-2xx",
		"instance=mimir-prod",
		"status=400",
		`body_preview="{\"error\":\"bad query\"}"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected log to contain %q, got %s", want, got)
		}
	}
}

func TestLogUpstreamNon2XXWarnsOnTooManyTenants422(t *testing.T) {
	logs := captureLogs(t, slog.LevelWarn)

	LogUpstreamNon2XX("mimir-prod", http.MethodGet, "http://mimir.local/api/v1/status/buildinfo", http.StatusUnprocessableEntity, time.Millisecond, "tenant-a|tenant-b", []byte("too many tenant IDs present in the request. max: 1 actual 2"), nil)

	got := logs.String()
	for _, want := range []string{
		"upstream returned non-2xx",
		"invalid configuration detected",
		"tenant-a|tenant-b",
		"too many tenant IDs present",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected log to contain %q, got %s", want, got)
		}
	}
}

func TestForwardQueryLogsAndReturnsUpstreamNon2XXBody(t *testing.T) {
	logs := captureLogs(t, slog.LevelWarn)
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnprocessableEntity,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("too many tenant IDs present in the request. max: 1 actual 2")),
			Request:    req,
		}, nil
	})}
	p := New(client, client)
	inst := &config.InstanceConfig{
		Name:    "mimir-prod",
		Backend: "mimir",
		URL:     "http://mimir.local",
	}

	req := httptest.NewRequest(http.MethodGet, "/prometheus/api/v1/status/buildinfo", nil)
	rec := httptest.NewRecorder()
	p.ForwardQuery(rec, req, inst, "/prometheus/api/v1/status/buildinfo")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, "too many tenant IDs present") {
		t.Fatalf("expected upstream body to be returned, got %q", got)
	}
	if got := logs.String(); !strings.Contains(got, "invalid configuration detected") {
		t.Fatalf("expected invalid configuration warning, got %s", got)
	}
}

func TestUpstreamErrorPreviewIsCapped(t *testing.T) {
	preview, truncated := upstreamErrorPreview([]byte(strings.Repeat("a", upstreamErrorBodyPreviewBytes+1)))

	if !truncated {
		t.Fatal("expected preview to be truncated")
	}
	if len(preview) != upstreamErrorBodyPreviewBytes {
		t.Fatalf("expected preview length %d, got %d", upstreamErrorBodyPreviewBytes, len(preview))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
