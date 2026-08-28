package otlpgrpc

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	collectlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collecttrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding/gzip"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/auth/authtest"
	"obsdatalayer/internal/authlimit"
	"obsdatalayer/internal/config"
	gwmetrics "obsdatalayer/internal/metrics"
	"obsdatalayer/internal/middleware"
	"obsdatalayer/internal/proxy"
)

type captureTransport struct {
	mu          sync.Mutex
	calls       int
	host        string
	path        string
	contentType string
	orgID       string
	body        []byte
}

func (t *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	t.host = req.URL.Host
	t.path = req.URL.Path
	t.contentType = req.Header.Get("Content-Type")
	t.orgID = req.Header.Get("X-Scope-OrgID")
	if req.Body != nil {
		t.body, _ = io.ReadAll(req.Body)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

func TestOTLPGRPCFallsBackToHTTPIngest(t *testing.T) {
	tests := []struct {
		name     string
		inst     *config.InstanceConfig
		call     func(context.Context, *grpc.ClientConn) error
		wantHost string
		wantPath string
	}{
		{
			name: "metrics",
			inst: &config.InstanceConfig{
				Name: "mimir-prod", Backend: "mimir",
				PushURLs: []config.PushTarget{{URL: "http://mimir.local", Group: config.TargetGroupPush}},
			},
			call: func(ctx context.Context, cc *grpc.ClientConn) error {
				_, err := collectmetrics.NewMetricsServiceClient(cc).Export(ctx, &collectmetrics.ExportMetricsServiceRequest{})
				return err
			},
			wantHost: "mimir.local",
			wantPath: "/otlp/v1/metrics",
		},
		{
			name: "logs",
			inst: &config.InstanceConfig{
				Name: "loki-prod", Backend: "loki",
				PushURLs: []config.PushTarget{{URL: "http://loki.local", Group: config.TargetGroupPush}},
			},
			call: func(ctx context.Context, cc *grpc.ClientConn) error {
				_, err := collectlogs.NewLogsServiceClient(cc).Export(ctx, &collectlogs.ExportLogsServiceRequest{})
				return err
			},
			wantHost: "loki.local",
			wantPath: "/otlp/v1/logs",
		},
		{
			name: "traces",
			inst: &config.InstanceConfig{
				Name: "tempo-prod", Backend: "tempo",
				PushURLs: []config.PushTarget{{URL: "http://tempo.local:4318", Group: config.TargetGroupOTLPHTTP}},
			},
			call: func(ctx context.Context, cc *grpc.ClientConn) error {
				_, err := collecttrace.NewTraceServiceClient(cc).Export(ctx, &collecttrace.ExportTraceServiceRequest{})
				return err
			},
			wantHost: "tempo.local:4318",
			wantPath: "/v1/traces",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			capture := &captureTransport{}
			cc := startGateway(t, testServer(t, []*config.InstanceConfig{tc.inst}, capture, authtest.New()))

			ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", authtest.New().Header()))
			if err := tc.call(ctx, cc); err != nil {
				t.Fatalf("export: %v", err)
			}

			if capture.calls != 1 {
				t.Fatalf("expected one HTTP fallback call, got %d", capture.calls)
			}
			if capture.host != tc.wantHost || capture.path != tc.wantPath {
				t.Fatalf("expected %s%s, got %s%s", tc.wantHost, tc.wantPath, capture.host, capture.path)
			}
			if capture.contentType != "application/x-protobuf" {
				t.Fatalf("expected OTLP protobuf content type, got %q", capture.contentType)
			}
			if capture.orgID != "test-tenant" {
				t.Fatalf("expected tenant header from grant, got %q", capture.orgID)
			}
		})
	}
}

func TestOTLPGRPCFallbackSendsParseableProtobuf(t *testing.T) {
	decoded := make(chan *collectmetrics.ExportMetricsServiceRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := &collectmetrics.ExportMetricsServiceRequest{}
		if err := proto.Unmarshal(body, req); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		decoded <- req
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	inst := &config.InstanceConfig{Name: "mimir-prod", Backend: "mimir", URL: upstream.URL}
	cc := startGateway(t, testServer(t, []*config.InstanceConfig{inst}, http.DefaultTransport, authtest.New()))
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", authtest.New().Header()))
	if _, err := collectmetrics.NewMetricsServiceClient(cc).Export(ctx, &collectmetrics.ExportMetricsServiceRequest{}); err != nil {
		t.Fatalf("export: %v", err)
	}

	select {
	case <-decoded:
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive a parseable OTLP protobuf request")
	}
}

func TestOTLPGRPCUsesExplicitGRPCTarget(t *testing.T) {
	upstream, captured := startUpstreamTrace(t)
	dial := func(ctx context.Context, target config.PushTarget) (*grpc.ClientConn, error) {
		return dialBufConn(ctx, upstream)
	}

	httpCapture := &captureTransport{}
	inst := &config.InstanceConfig{
		Name: "tempo-prod", Backend: "tempo",
		PushURLs: []config.PushTarget{
			{URL: "http://tempo-grpc.local:4317", Group: config.TargetGroupOTLPGRPC, BasicAuth: "upstream:secret"},
			{URL: "http://tempo-http.local:4318", Group: config.TargetGroupOTLPHTTP},
		},
	}
	cc := startGateway(t, testServerWithDial(t, []*config.InstanceConfig{inst}, httpCapture, authtest.New(), dial))
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", authtest.New().Header()))

	if _, err := collecttrace.NewTraceServiceClient(cc).Export(ctx, &collecttrace.ExportTraceServiceRequest{}); err != nil {
		t.Fatalf("export: %v", err)
	}

	if httpCapture.calls != 0 {
		t.Fatalf("explicit otlp_grpc target should not use HTTP fallback, got %d calls", httpCapture.calls)
	}
	if captured.calls != 1 {
		t.Fatalf("expected one upstream gRPC call, got %d", captured.calls)
	}
	if got := firstMetadataValue(captured.md, "x-scope-orgid"); got != "test-tenant" {
		t.Fatalf("expected tenant metadata, got %q", got)
	}
	if got := firstMetadataValue(captured.md, "authorization"); got != authtest.BasicHeader("upstream", "secret") {
		t.Fatalf("expected upstream basic auth, got %q", got)
	}
}

func TestOTLPGRPCCachesUpstreamConnections(t *testing.T) {
	upstream, _ := startUpstreamTrace(t)
	var dials atomic.Int32
	dial := func(ctx context.Context, target config.PushTarget) (*grpc.ClientConn, error) {
		dials.Add(1)
		return dialBufConn(ctx, upstream)
	}
	inst := &config.InstanceConfig{
		Name: "tempo-prod", Backend: "tempo",
		PushURLs: []config.PushTarget{{URL: "http://tempo-grpc.local:4317", Group: config.TargetGroupOTLPGRPC}},
	}
	cc := startGateway(t, testServerWithDial(t, []*config.InstanceConfig{inst}, &captureTransport{}, authtest.New(), dial))
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", authtest.New().Header()))

	client := collecttrace.NewTraceServiceClient(cc)
	for i := 0; i < 2; i++ {
		if _, err := client.Export(ctx, &collecttrace.ExportTraceServiceRequest{}); err != nil {
			t.Fatalf("export %d: %v", i, err)
		}
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("expected one dial for cached upstream connection, got %d", got)
	}
}

func TestOTLPGRPCAnyModeReportsPartialFailureTrailer(t *testing.T) {
	okUpstream, _ := startUpstreamTrace(t)
	failUpstream := startFailingTrace(t, codes.Unavailable)
	dial := func(ctx context.Context, target config.PushTarget) (*grpc.ClientConn, error) {
		if strings.Contains(target.URL, "fail") {
			return dialBufConn(ctx, failUpstream)
		}
		return dialBufConn(ctx, okUpstream)
	}
	inst := &config.InstanceConfig{
		Name: "tempo-prod", Backend: "tempo", FanOutMode: "any",
		PushURLs: []config.PushTarget{
			{URL: "http://tempo-ok.local:4317", Group: config.TargetGroupOTLPGRPC},
			{URL: "http://tempo-fail.local:4317", Group: config.TargetGroupOTLPGRPC},
		},
	}
	cc := startGateway(t, testServerWithDial(t, []*config.InstanceConfig{inst}, &captureTransport{}, authtest.New(), dial))
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", authtest.New().Header()))
	var trailer metadata.MD

	if _, err := collecttrace.NewTraceServiceClient(cc).Export(ctx, &collecttrace.ExportTraceServiceRequest{}, grpc.Trailer(&trailer)); err != nil {
		t.Fatalf("export: %v", err)
	}
	if got := firstMetadataValue(trailer, "x-gateway-partial-failure"); !strings.Contains(got, "status=503") {
		t.Fatalf("expected partial-failure trailer with status=503, got %q", got)
	}
}

func TestOTLPGRPCAllModeFailsOnOneTargetFailure(t *testing.T) {
	okUpstream, _ := startUpstreamTrace(t)
	failUpstream := startFailingTrace(t, codes.Unavailable)
	dial := func(ctx context.Context, target config.PushTarget) (*grpc.ClientConn, error) {
		if strings.Contains(target.URL, "fail") {
			return dialBufConn(ctx, failUpstream)
		}
		return dialBufConn(ctx, okUpstream)
	}
	inst := &config.InstanceConfig{
		Name: "tempo-prod", Backend: "tempo", FanOutMode: "all",
		PushURLs: []config.PushTarget{
			{URL: "http://tempo-ok.local:4317", Group: config.TargetGroupOTLPGRPC},
			{URL: "http://tempo-fail.local:4317", Group: config.TargetGroupOTLPGRPC},
		},
	}
	cc := startGateway(t, testServerWithDial(t, []*config.InstanceConfig{inst}, &captureTransport{}, authtest.New(), dial))
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", authtest.New().Header()))

	_, err := collecttrace.NewTraceServiceClient(cc).Export(ctx, &collecttrace.ExportTraceServiceRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected unavailable from all-mode failure, got %v", err)
	}
}

func TestOTLPGRPCAcceptsGzipCompression(t *testing.T) {
	capture := &captureTransport{}
	inst := &config.InstanceConfig{Name: "mimir-prod", Backend: "mimir", URL: "http://mimir.local"}
	cc := startGateway(t, testServer(t, []*config.InstanceConfig{inst}, capture, authtest.New()))

	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", authtest.New().Header()))
	_, err := collectmetrics.NewMetricsServiceClient(cc).Export(ctx, &collectmetrics.ExportMetricsServiceRequest{}, grpc.UseCompressor(gzip.Name))
	if err != nil {
		t.Fatalf("export with gzip compression: %v", err)
	}
	if capture.calls != 1 {
		t.Fatalf("expected compressed request to reach HTTP fallback, got %d calls", capture.calls)
	}
}

func TestOTLPGRPCTransportRejectsOversizeBeforeAuth(t *testing.T) {
	capture := &captureTransport{}
	inst := &config.InstanceConfig{Name: "loki-prod", Backend: "loki", URL: "http://loki.local"}
	cc := startGateway(t, testServerWithConfig(t, []*config.InstanceConfig{inst}, capture, authtest.New(), nil, config.GatewayConfig{
		MaxBodyBytes:         1024 * 1024,
		DefaultTargetTimeout: config.Duration(time.Second),
	}))

	req := &collectlogs.ExportLogsServiceRequest{
		ResourceLogs: []*logsv1.ResourceLogs{{
			ScopeLogs: []*logsv1.ScopeLogs{{
				LogRecords: []*logsv1.LogRecord{{
					Body: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: strings.Repeat("x", 2*1024*1024)}},
				}},
			}},
		}},
	}
	_, err := collectlogs.NewLogsServiceClient(cc).Export(context.Background(), req)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected transport ResourceExhausted before auth, got %v", err)
	}
	if capture.calls != 0 {
		t.Fatalf("oversize request should not reach upstream, got %d calls", capture.calls)
	}
}

func TestOTLPGRPCAuthRequired(t *testing.T) {
	capture := &captureTransport{}
	inst := &config.InstanceConfig{Name: "mimir-prod", Backend: "mimir", URL: "http://mimir.local"}
	cc := startGateway(t, testServer(t, []*config.InstanceConfig{inst}, capture, authtest.New()))

	_, err := collectmetrics.NewMetricsServiceClient(cc).Export(context.Background(), &collectmetrics.ExportMetricsServiceRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected unauthenticated, got %v", err)
	}
	if capture.calls != 0 {
		t.Fatalf("unauthenticated request should not reach upstream, got %d calls", capture.calls)
	}
}

func TestOTLPGRPCAuthThrottleAddsRetryInfo(t *testing.T) {
	stub := authtest.New()
	guard := &middleware.AuthGuard{
		Limiter: authlimit.NewLimiter(authlimit.Config{
			Enabled:          true,
			FailureThreshold: 1,
			FailureWindow:    time.Minute,
			BlockDuration:    5 * time.Second,
			MaxBlockDuration: 5 * time.Second,
		}),
	}
	capture := &captureTransport{}
	inst := &config.InstanceConfig{Name: "mimir-prod", Backend: "mimir", URL: "http://mimir.local"}
	cc := startGateway(t, testServerWithConfig(t, []*config.InstanceConfig{inst}, capture, stub, guard, config.GatewayConfig{
		DefaultTargetTimeout: config.Duration(time.Second),
	}))
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", authtest.BasicHeader("testuser", "wrong")))
	client := collectmetrics.NewMetricsServiceClient(cc)

	if _, err := client.Export(ctx, &collectmetrics.ExportMetricsServiceRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("first bad credential should fail auth, got %v", err)
	}
	_, err := client.Export(ctx, &collectmetrics.ExportMetricsServiceRequest{})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected throttled request, got %v", err)
	}
	st, _ := status.FromError(err)
	var sawRetry bool
	for _, detail := range st.Details() {
		if _, ok := detail.(*errdetails.RetryInfo); ok {
			sawRetry = true
		}
	}
	if !sawRetry {
		t.Fatalf("expected RetryInfo detail on throttled error, got %#v", st.Details())
	}
}

func TestOTLPGRPCRequiresWriteGrant(t *testing.T) {
	stub := authtest.New()
	stub.Allow = map[string]bool{"mimir:" + auth.ActionWrite: false}
	capture := &captureTransport{}
	inst := &config.InstanceConfig{Name: "mimir-prod", Backend: "mimir", URL: "http://mimir.local"}
	cc := startGateway(t, testServer(t, []*config.InstanceConfig{inst}, capture, stub))

	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", stub.Header()))
	_, err := collectmetrics.NewMetricsServiceClient(cc).Export(ctx, &collectmetrics.ExportMetricsServiceRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected permission denied, got %v", err)
	}
	if capture.calls != 0 {
		t.Fatalf("denied request should not reach upstream, got %d calls", capture.calls)
	}
}

func TestOTLPGRPCRejectsAmbiguousWriteTenant(t *testing.T) {
	stub := authtest.New()
	stub.Tenants = []string{"tenant-a", "tenant-b"}
	capture := &captureTransport{}
	inst := &config.InstanceConfig{Name: "mimir-prod", Backend: "mimir", URL: "http://mimir.local"}
	cc := startGateway(t, testServer(t, []*config.InstanceConfig{inst}, capture, stub))
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", stub.Header()))

	_, err := collectmetrics.NewMetricsServiceClient(cc).Export(ctx, &collectmetrics.ExportMetricsServiceRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected permission denied for ambiguous write tenant, got %v", err)
	}
	if capture.calls != 0 {
		t.Fatalf("ambiguous write should not reach upstream, got %d calls", capture.calls)
	}
}

func TestOTLPGRPCRejectsAmbiguousInstance(t *testing.T) {
	capture := &captureTransport{}
	insts := []*config.InstanceConfig{
		{Name: "mimir-a", Backend: "mimir", URL: "http://mimir-a.local"},
		{Name: "mimir-b", Backend: "mimir", URL: "http://mimir-b.local"},
	}
	cc := startGateway(t, testServer(t, insts, capture, authtest.New()))
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", authtest.New().Header()))

	_, err := collectmetrics.NewMetricsServiceClient(cc).Export(ctx, &collectmetrics.ExportMetricsServiceRequest{})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected failed precondition for ambiguous instance, got %v", err)
	}
	if capture.calls != 0 {
		t.Fatalf("ambiguous instance should not reach upstream, got %d calls", capture.calls)
	}
}

func TestOTLPGRPCHealthService(t *testing.T) {
	cc := startGateway(t, testServer(t, nil, &captureTransport{}, authtest.New()))

	resp, err := healthpb.NewHealthClient(cc).Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("expected SERVING, got %s", resp.Status)
	}
}

func TestOTLPGRPCHealthSetNotServing(t *testing.T) {
	srv := testServer(t, nil, &captureTransport{}, authtest.New())
	cc := startGateway(t, srv)

	srv.SetNotServing()
	resp, err := healthpb.NewHealthClient(cc).Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	if resp.Status != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("expected NOT_SERVING, got %s", resp.Status)
	}
}

func TestHTTPFallbackRetryAfterBecomesRetryInfo(t *testing.T) {
	capture := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": []string{"7"}},
			Body:       io.NopCloser(strings.NewReader("rate limited")),
			Request:    req,
		}, nil
	})
	inst := &config.InstanceConfig{Name: "mimir-prod", Backend: "mimir", URL: "http://mimir.local"}
	cc := startGateway(t, testServer(t, []*config.InstanceConfig{inst}, capture, authtest.New()))
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", authtest.New().Header()))

	_, err := collectmetrics.NewMetricsServiceClient(cc).Export(ctx, &collectmetrics.ExportMetricsServiceRequest{})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted, got %v", err)
	}
	st, _ := status.FromError(err)
	var retry *errdetails.RetryInfo
	for _, detail := range st.Details() {
		if ri, ok := detail.(*errdetails.RetryInfo); ok {
			retry = ri
		}
	}
	if retry == nil || retry.RetryDelay.AsDuration() != 7*time.Second {
		t.Fatalf("expected RetryInfo delay of 7s, got %#v", st.Details())
	}
}

func TestDialTargetRejectsUnsupportedSchemeAndPath(t *testing.T) {
	if _, err := dialTarget(context.Background(), config.PushTarget{URL: "ftp://tempo.local:4317"}); err == nil {
		t.Fatal("expected unsupported scheme to fail")
	}
	if _, err := dialTarget(context.Background(), config.PushTarget{URL: "http://tempo.local:4317/path"}); err == nil {
		t.Fatal("expected path-bearing target URL to fail")
	}
}

func testServer(t *testing.T, insts []*config.InstanceConfig, rt http.RoundTripper, a auth.Authorizer) *Server {
	t.Helper()
	return testServerWithDial(t, insts, rt, a, dialTarget)
}

func testServerWithDial(t *testing.T, insts []*config.InstanceConfig, rt http.RoundTripper, a auth.Authorizer, dial targetDialer) *Server {
	t.Helper()
	return testServerWithConfig(t, insts, rt, a, nil, config.GatewayConfig{DefaultTargetTimeout: config.Duration(time.Second)}, dial)
}

func testServerWithConfig(t *testing.T, insts []*config.InstanceConfig, rt http.RoundTripper, a auth.Authorizer, guard *middleware.AuthGuard, gateway config.GatewayConfig, dialers ...targetDialer) *Server {
	t.Helper()
	cfg, err := config.New(&config.Config{
		Gateway:   gateway,
		Instances: insts,
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	client := &http.Client{Timeout: time.Second, Transport: rt}
	p := proxy.New(client, client)
	dial := dialTarget
	if len(dialers) > 0 {
		dial = dialers[0]
	}
	return newServerWithDial(config.NewHolder(cfg, ""), a, p, gwmetrics.New(prometheus.NewRegistry()), guard, dial)
}

func startGateway(t *testing.T, srv *Server) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})
	cc, err := grpc.DialContext(
		context.Background(),
		"bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

type captureTraceServer struct {
	collecttrace.UnimplementedTraceServiceServer
	calls int
	md    metadata.MD
}

func (s *captureTraceServer) Export(ctx context.Context, _ *collecttrace.ExportTraceServiceRequest) (*collecttrace.ExportTraceServiceResponse, error) {
	s.calls++
	s.md, _ = metadata.FromIncomingContext(ctx)
	return &collecttrace.ExportTraceServiceResponse{}, nil
}

func startUpstreamTrace(t *testing.T) (*bufconn.Listener, *captureTraceServer) {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	captured := &captureTraceServer{}
	collecttrace.RegisterTraceServiceServer(srv, captured)
	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})
	return lis, captured
}

type failingTraceServer struct {
	collecttrace.UnimplementedTraceServiceServer
	code codes.Code
}

func (s failingTraceServer) Export(context.Context, *collecttrace.ExportTraceServiceRequest) (*collecttrace.ExportTraceServiceResponse, error) {
	return nil, status.Error(s.code, "upstream failed")
}

func startFailingTrace(t *testing.T, code codes.Code) *bufconn.Listener {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	collecttrace.RegisterTraceServiceServer(srv, failingTraceServer{code: code})
	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})
	return lis
}

func dialBufConn(ctx context.Context, lis *bufconn.Listener) (*grpc.ClientConn, error) {
	return grpc.DialContext(
		ctx,
		"bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
