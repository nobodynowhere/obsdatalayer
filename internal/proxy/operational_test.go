package proxy_test

import (
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/config"
	"obsdatalayer/internal/proxy"
)

// TestCopyHeadersUntenantedNeverSetsOrgID is the structural half of the
// guarantee. The tenanted copier falls back to the target's configured tenant
// ID when the request carries no grant tenants, which is exactly how an
// operational request would silently reacquire a tenant assertion if it called
// the wrong one. This copier has no such path.
func TestCopyHeadersUntenantedNeverSetsOrgID(t *testing.T) {
	target := config.PushTarget{URL: "http://upstream.local", TenantID: "tenant-a", BasicAuth: "user:pass"}

	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{name: "no auth context", ctx: context.Background()},
		{
			name: "grant carrying tenants",
			ctx: auth.WithRequestAuth(context.Background(), &auth.RequestAuth{
				Username: "alice", TenantIDs: []string{"tenant-a", "tenant-b"}, IsRead: true,
			}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(tc.ctx, http.MethodGet, target.URL+"/ready", nil)
			if err != nil {
				t.Fatal(err)
			}
			inbound := http.Header{
				"Accept":        []string{"application/json"},
				"X-Scope-Orgid": []string{"EVIL"},
				"Cookie":        []string{"session=abc"},
			}
			proxy.CopyHeadersUntenanted(req, inbound, target)

			if org := req.Header.Get("X-Scope-OrgID"); org != "" {
				t.Fatalf("X-Scope-OrgID = %q, want it absent", org)
			}
			if req.Header.Get("Cookie") != "" {
				t.Error("Cookie is not forwardable and must be dropped")
			}
			if req.Header.Get("Accept") != "application/json" {
				t.Error("Accept is forwardable and should have survived")
			}
			// The target's own credential still goes on: it is how the gateway
			// authenticates to the backend, not a tenant claim.
			if user, pass, ok := req.BasicAuth(); !ok || user != "user" || pass != "pass" {
				t.Error("the target's basic auth credential should still be injected")
			}
		})
	}

	// And the tenanted copier still does inject, so this test is testing a
	// difference rather than a coincidence.
	req, _ := http.NewRequest(http.MethodGet, target.URL+"/query", nil)
	proxy.CopyHeadersForUpstream(req, nil, target)
	if req.Header.Get("X-Scope-OrgID") != "tenant-a" {
		t.Fatal("CopyHeadersForUpstream must still inject the target's tenant")
	}
}

func TestFetchOperationalReturnsUpstreamAnswer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("ingester not ready"))
	}))
	t.Cleanup(upstream.Close)

	p := proxy.New(upstream.Client(), upstream.Client())
	inst := &config.InstanceConfig{Name: "loki-prod", Backend: "loki", URL: upstream.URL}
	target := config.PushTarget{URL: upstream.URL}

	got, err := p.FetchOperational(context.Background(), inst, target, "ready", "/ready", "", nil, 0)
	// A 503 is an answer, not a failure: it is the observation being asked for.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", got.StatusCode)
	}
	if string(got.Body) != "ingester not ready" {
		t.Fatalf("body = %q", got.Body)
	}
	if !strings.HasPrefix(got.ContentType, "text/plain") {
		t.Fatalf("content type = %q", got.ContentType)
	}
	if got.Truncated {
		t.Error("a short body must not report truncated")
	}
}

func TestFetchOperationalCapsTheBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("m", 5000)))
	}))
	t.Cleanup(upstream.Close)

	p := proxy.New(upstream.Client(), upstream.Client())
	inst := &config.InstanceConfig{Name: "loki-prod", Backend: "loki", URL: upstream.URL}

	got, err := p.FetchOperational(context.Background(), inst, config.PushTarget{URL: upstream.URL}, "metrics", "/metrics", "", nil, 1000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Body) != 1000 {
		t.Fatalf("body is %d bytes, want 1000", len(got.Body))
	}
	if !got.Truncated {
		t.Fatal("a capped body must report truncated")
	}
}

func TestFetchOperationalHonoursTheTargetTimeout(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(func() { close(release); upstream.Close() })

	p := proxy.New(upstream.Client(), upstream.Client())
	inst := &config.InstanceConfig{Name: "loki-prod", Backend: "loki", URL: upstream.URL}
	target := config.PushTarget{URL: upstream.URL, TimeoutSeconds: 0}
	p.SetDefaultTargetTimeout(50 * time.Millisecond)

	_, err := p.FetchOperational(context.Background(), inst, target, "ready", "/ready", "", nil, 0)
	if err == nil {
		t.Fatal("a hung target must produce an error, not hang the fan-out")
	}
	if !proxy.IsTimeout(err) {
		t.Fatalf("error = %v, want a timeout", err)
	}
}

// TestFetchOperationalReturnsDecodedTextWhenTheCallerAcceptsGzip covers the
// bug the operational /metrics view had: a browser offers "gzip, deflate, br,
// zstd" on every request, Accept-Encoding is on the forwardable allowlist, and
// promhttp takes the offer. The compressed bytes then reached the fan-out,
// which put them in a JSON string, which the UI rendered as mojibake. Loki's
// plainer handlers -- /ready, /config -- ignore the offer, which is why only
// metrics looked corrupt.
//
// The assertion is on the returned body, not on the outbound header, because
// what callers depend on is that this returns text: whether Go's transport
// negotiates gzip underneath is its own business.
func TestFetchOperationalReturnsDecodedTextWhenTheCallerAcceptsGzip(t *testing.T) {
	const exposition = "# HELP loki_build_info Build information.\nloki_build_info{version=\"3.4.1\"} 1\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			_, _ = w.Write([]byte(exposition))
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte(exposition))
		_ = gz.Close()
	}))
	t.Cleanup(upstream.Close)

	p := proxy.New(upstream.Client(), upstream.Client())
	inst := &config.InstanceConfig{Name: "loki-prod", Backend: "loki", URL: upstream.URL}
	inbound := http.Header{"Accept-Encoding": []string{"gzip, deflate, br, zstd"}}

	got, err := p.FetchOperational(
		context.Background(), inst, config.PushTarget{URL: upstream.URL},
		"metrics", "/metrics", "", inbound, 1<<20,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got.Body) != exposition {
		t.Fatalf("body = %q, want the decoded exposition", string(got.Body))
	}
	// The relayed header must not keep describing an encoding the body no
	// longer carries: the legacy passthrough copies this header verbatim onto
	// its own response, and a Content-Encoding: gzip over plain text is a
	// response no client can read.
	if enc := got.Header.Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q, want it absent once the body is decoded", enc)
	}
}
