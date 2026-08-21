package proxy_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
