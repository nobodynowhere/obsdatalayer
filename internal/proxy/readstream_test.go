package proxy_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"obsdatalayer/internal/metrics"
)

// labelValuesJSON builds a realistic /api/v1/label/__name__/values reply: large
// enough to exceed the transport's read buffer, so a body that is streamed to
// the client rather than buffered is the only way it arrives intact.
func labelValuesJSON(n int) string {
	vals := make([]string, n)
	for i := range vals {
		vals[i] = fmt.Sprintf("some_service_metric_total_number_%05d", i)
	}
	b, _ := json.Marshal(map[string]any{"status": "success", "data": vals})
	return string(b)
}

func readLargeBody(t *testing.T, upstreamURLs ...string) *httptest.ResponseRecorder {
	t.Helper()
	p := newProxy(&http.Client{Timeout: 30 * time.Second})
	// A per-target timeout is what production always has; without one the bug
	// this guards against cannot occur, because there is no context to cancel.
	p.SetDefaultTargetTimeout(30 * time.Second)

	const path = "/prometheus/api/v1/label/__name__/values"
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	p.ForwardQuery(rec, req, fanoutInstance("mimir-ha", upstreamURLs...), path)
	return rec
}

// The defect: attemptRead cancelled its per-target timeout context on return,
// while the response body was still open and unread. The connection died under
// commit's io.Copy, so anything past the transport's buffer was dropped and the
// client received a 200 with a truncated body -- a JSON parse error in Grafana
// on exactly the endpoints whose replies are large.
func TestLargeReadBodyIsNotTruncated(t *testing.T) {
	want := labelValuesJSON(5000)
	up := newTarget(t, http.StatusOK, want)

	rec := readLargeBody(t, up.srv.URL)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != want {
		t.Fatalf("body truncated: got %d bytes, want %d", len(got), len(want))
	}
	var decoded any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
}

// The same guarantee on the attempt that had to fail over first, which is the
// one whose context lifetime is easiest to get wrong.
func TestLargeReadBodyIsNotTruncatedAfterFailover(t *testing.T) {
	want := labelValuesJSON(5000)
	broken := newTarget(t, http.StatusBadGateway, "upstream is unwell")
	live := newTarget(t, http.StatusOK, want)

	rec := readLargeBody(t, broken.srv.URL, live.srv.URL)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != want {
		t.Fatalf("body truncated: got %d bytes, want %d", len(got), len(want))
	}
}

// shortBodyTarget promises a Content-Length it does not deliver, then drops the
// connection. That is what a genuinely broken upstream looks like from the
// gateway: headers and a status arrive, the body stops part-way.
func shortBodyTarget(t *testing.T, promised int, send string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 4096)
				_, _ = conn.Read(buf) // consume the request line and headers
				fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", promised)
				_, _ = conn.Write([]byte(send))
			}()
		}
	}()
	return "http://" + ln.Addr().String()
}

// A body that stops part-way cannot be reported in the response: the 200 is
// already on the wire. It must therefore show up in the log and the counters,
// or the gateway hands the client a silent partial result.
func TestTruncatedReadBodyIsLoggedAndCounted(t *testing.T) {
	full := labelValuesJSON(5000)
	upstream := shortBodyTarget(t, len(full), full[:1000])

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	m := metrics.New(prometheus.NewRegistry())
	p := newProxy(&http.Client{Timeout: 30 * time.Second})
	p.SetDefaultTargetTimeout(30 * time.Second)
	p.SetMetrics(m)

	const path = "/prometheus/api/v1/label/__name__/values"
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	p.ForwardQuery(rec, req, fanoutInstance("mimir-ha", upstream), path)

	if got := m.ReadTruncatedValue("mimir-ha", upstream); got != 1 {
		t.Errorf("gateway_read_truncated_total = %d, want 1", got)
	}
	if !strings.Contains(logs.String(), "read response body truncated") {
		t.Errorf("expected an error log for the truncated body, got:\n%s", logs.String())
	}
	// The upstream is still healthy from the status line's point of view, so the
	// read counts as a success. Counting a failure here would park a working
	// replica in the cool-off over a body that the upstream may not be at fault
	// for at all.
	if got := m.ReadValue("mimir-ha", upstream, "success"); got != 1 {
		t.Errorf("read successes = %d, want 1", got)
	}
}

// The counter is labeled by target base URL, not by the full upstream URL: the
// latter carries the caller's query string and would mint a series per query.
func TestTruncationCounterIsNotLabeledByQueryString(t *testing.T) {
	full := labelValuesJSON(5000)
	upstream := shortBodyTarget(t, len(full), full[:1000])

	m := metrics.New(prometheus.NewRegistry())
	p := newProxy(&http.Client{Timeout: 30 * time.Second})
	p.SetDefaultTargetTimeout(30 * time.Second)
	p.SetMetrics(m)

	const path = "/prometheus/api/v1/label/__name__/values"
	for _, q := range []string{"?start=1", "?start=2", "?start=3"} {
		req := httptest.NewRequest(http.MethodGet, path+q, nil)
		p.ForwardQuery(httptest.NewRecorder(), req, fanoutInstance("mimir-ha", upstream), path)
	}

	if got := m.ReadTruncatedValue("mimir-ha", upstream); got != 3 {
		t.Errorf("gateway_read_truncated_total = %d, want 3 on one series", got)
	}
}
