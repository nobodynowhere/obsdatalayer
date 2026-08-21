package proxy_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"obsdatalayer/internal/config"
	"obsdatalayer/internal/metrics"
	"obsdatalayer/internal/proxy"
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

// rawTarget answers on a raw socket so a test can frame a reply the way a
// broken upstream would: headers and a status arrive, the body stops part-way.
// reply receives the connection after the request has been consumed.
func rawTarget(t *testing.T, reply func(conn net.Conn)) string {
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
				_, _ = conn.Read(make([]byte, 4096)) // request line and headers
				reply(conn)
			}()
		}
	}()
	return "http://" + ln.Addr().String()
}

// declaredLengthShortTarget promises a Content-Length it does not deliver.
func declaredLengthShortTarget(t *testing.T, promised int, send string) string {
	t.Helper()
	return rawTarget(t, func(conn net.Conn) {
		fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", promised)
		_, _ = conn.Write([]byte(send))
	})
}

// chunkedShortTarget sends chunks and then hangs up without the terminating
// zero-length chunk. This is the quiet case: nothing in the framing tells the
// gateway's client how much was still to come.
func chunkedShortTarget(t *testing.T, send string) string {
	t.Helper()
	return rawTarget(t, func(conn net.Conn) {
		fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n")
		fmt.Fprintf(conn, "%x\r\n%s\r\n", len(send), send)
	})
}

func truncatingProxy(t *testing.T) (*proxy.Proxy, *metrics.Metrics) {
	t.Helper()
	m := metrics.New(prometheus.NewRegistry())
	p := newProxy(&http.Client{Timeout: 30 * time.Second})
	p.SetDefaultTargetTimeout(30 * time.Second)
	p.SetMetrics(m)
	return p, m
}

const truncPath = "/prometheus/api/v1/label/__name__/values"

// forwardExpectingAbort runs one read that is expected to abort its handler,
// and reports whether it did. net/http recovers this panic and drops the
// connection; a test driving ForwardQuery directly has to recover it itself.
func forwardExpectingAbort(p *proxy.Proxy, inst *config.InstanceConfig, query string) (aborted bool) {
	defer func() {
		if rec := recover(); rec != nil {
			aborted = rec == http.ErrAbortHandler
		}
	}()
	req := httptest.NewRequest(http.MethodGet, truncPath+query, nil)
	p.ForwardQuery(httptest.NewRecorder(), req, inst, truncPath)
	return false
}

// A body that stops part-way cannot be reported in the response: the 200 is
// already on the wire. It must therefore show up in the log and the counters,
// or the gateway hands the client a silent partial result.
func TestTruncatedReadBodyIsLoggedAndCounted(t *testing.T) {
	full := labelValuesJSON(5000)
	upstream := declaredLengthShortTarget(t, len(full), full[:1000])

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	p, m := truncatingProxy(t)
	if !forwardExpectingAbort(p, fanoutInstance("mimir-ha", upstream), "") {
		t.Fatal("expected the handler to abort on a truncated body")
	}

	if got := m.ReadTruncatedValue("mimir-ha", upstream); got != 1 {
		t.Errorf("gateway_read_truncated_total = %d, want 1", got)
	}
	if !strings.Contains(logs.String(), "read response body truncated") {
		t.Errorf("expected an error log for the truncated body, got:\n%s", logs.String())
	}
}

// The counter is labeled by target base URL, not by the full upstream URL: the
// latter carries the caller's query string and would mint a series per query.
func TestTruncationCounterIsNotLabeledByQueryString(t *testing.T) {
	full := labelValuesJSON(5000)
	upstream := declaredLengthShortTarget(t, len(full), full[:1000])

	p, m := truncatingProxy(t)
	inst := fanoutInstance("mimir-ha", upstream)
	for _, q := range []string{"?start=1", "?start=2", "?start=3"} {
		if !forwardExpectingAbort(p, inst, q) {
			t.Fatalf("expected the handler to abort for %s", q)
		}
	}

	if got := m.ReadTruncatedValue("mimir-ha", upstream); got != 3 {
		t.Errorf("gateway_read_truncated_total = %d, want 3 on one series", got)
	}
}

// A truncated body is not a failover-class error. The status line has gone out,
// so no replica's answer could be sent anyway, and the target answered the
// request it was given -- failing it here would park a healthy replica in the
// read cool-off over a copy that may have broken on the client's side.
func TestTruncatedBodyDoesNotFailOverOrFailTheTarget(t *testing.T) {
	full := labelValuesJSON(5000)
	first := declaredLengthShortTarget(t, len(full), full[:1000])
	second := newTarget(t, http.StatusOK, full)

	p, m := truncatingProxy(t)
	if !forwardExpectingAbort(p, fanoutInstance("mimir-ha", first, second.srv.URL), "") {
		t.Fatal("expected the handler to abort on a truncated body")
	}

	if got := second.calls.Load(); got != 0 {
		t.Errorf("second target was asked %d times; a truncated body must not fail over", got)
	}
	if got := m.ReadFailoverValue("mimir-ha"); got != 0 {
		t.Errorf("read failovers = %d, want 0", got)
	}
	if got := m.ReadValue("mimir-ha", first, "failure"); got != 0 {
		t.Errorf("read failures for the serving target = %d, want 0", got)
	}
	if got := m.ReadValue("mimir-ha", first, "success"); got != 1 {
		t.Errorf("read successes for the serving target = %d, want 1", got)
	}
}

// The case the abort exists for. When the upstream answered chunked the gateway
// answers chunked too, so simply returning would have Go write the terminating
// chunk and hand the caller a complete-looking reply that is missing data.
func TestTruncatedChunkedBodyIsNotDeliveredAsCompleteResponse(t *testing.T) {
	upstream := chunkedShortTarget(t, labelValuesJSON(5000)[:1000])

	p, _ := truncatingProxy(t)
	inst := fanoutInstance("mimir-ha", upstream)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.ForwardQuery(w, r, inst, truncPath)
	}))
	t.Cleanup(gw.Close)

	resp, err := gw.Client().Get(gw.URL + truncPath)
	if err != nil {
		return // the client rejected the reply outright, which is also a hard failure
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("client read a clean %d-byte body and was never told it was incomplete; "+
			"status was %d", len(body), resp.StatusCode)
	}
}
