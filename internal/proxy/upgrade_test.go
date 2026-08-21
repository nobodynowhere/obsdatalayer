package proxy

import (
	"bufio"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/config"
)

// switchingUpstream is a Loki stand-in that completes the handshake by hand:
// hijacking and writing the status line is what makes ReverseProxy take its
// upgrade path, and no WebSocket framing is needed to get there.
func switchingUpstream(t *testing.T, status string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("upstream ResponseWriter is not a Hijacker")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("upstream hijack: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString(status + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = buf.Flush()
	}))
}

// tailThroughGateway drives one live-tail handshake through ForwardUpgrade and
// returns once the gateway has answered. The client is a raw connection: the
// point is the 101, which is not a response an http.Client hands back cleanly.
func tailThroughGateway(t *testing.T, upstreamURL string) {
	t.Helper()

	p := New(NewHTTPClient(5*time.Second), NewHTTPClient(5*time.Second))
	inst := &config.InstanceConfig{Name: "loki-prod", Backend: "loki", URL: upstreamURL}

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ra := &auth.RequestAuth{Username: "tailer", TenantIDs: []string{"tenant-a"}, IsRead: true}
		p.ForwardUpgrade(w, r.WithContext(auth.WithRequestAuth(r.Context(), ra)), inst, "/loki/api/v1/tail")
	}))
	defer gateway.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(gateway.URL, "http://"))
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_, err = conn.Write([]byte(
		"GET /loki/api/v1/tail?query=%7Bapp%3D%22web%22%7D HTTP/1.1\r\n" +
			"Host: gateway\r\n" +
			"Connection: Upgrade\r\n" +
			"Upgrade: websocket\r\n" +
			"Sec-Websocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
			"Sec-Websocket-Version: 13\r\n\r\n"))
	if err != nil {
		t.Fatalf("write handshake: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		t.Fatalf("read gateway response: %v", err)
	}
}

func TestForwardUpgradeLogsSuccessfulTailAtInfo(t *testing.T) {
	logs := captureLogs(t, slog.LevelInfo)

	upstream := switchingUpstream(t, "HTTP/1.1 101 Switching Protocols")
	defer upstream.Close()

	tailThroughGateway(t, upstream.URL)

	got := logs.String()
	for _, want := range []string{
		"loki tail upgraded to websocket",
		"instance=loki-prod",
		"user=tailer",
		"org_id=tenant-a",
		"path=/loki/api/v1/tail",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected log to contain %q, got:\n%s", want, got)
		}
	}
}

// An upstream that refuses the handshake is not an upgrade, so it must not
// produce the line an operator reads as "a tail is running".
func TestForwardUpgradeDoesNotLogWhenUpstreamRefuses(t *testing.T) {
	logs := captureLogs(t, slog.LevelInfo)

	upstream := switchingUpstream(t, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0")
	defer upstream.Close()

	tailThroughGateway(t, upstream.URL)

	if got := logs.String(); strings.Contains(got, "loki tail upgraded to websocket") {
		t.Fatalf("logged an upgrade for a refused handshake:\n%s", got)
	}
}

// The existing debug tracing is untouched by the new info line: both are
// emitted when the level allows it.
func TestForwardUpgradeKeepsDebugTracing(t *testing.T) {
	logs := captureLogs(t, slog.LevelDebug)

	upstream := switchingUpstream(t, "HTTP/1.1 101 Switching Protocols")
	defer upstream.Close()

	tailThroughGateway(t, upstream.URL)

	got := logs.String()
	for _, want := range []string{"forwarding upgrade upstream", "loki tail upgraded to websocket"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected log to contain %q, got:\n%s", want, got)
		}
	}
}
