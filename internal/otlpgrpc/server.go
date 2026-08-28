// Package otlpgrpc accepts OTLP/gRPC telemetry and forwards it to configured
// backend receivers.
package otlpgrpc

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	collectlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collecttrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	_ "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"obsdatalayer/internal/auth"
	"obsdatalayer/internal/authlimit"
	"obsdatalayer/internal/config"
	"obsdatalayer/internal/fanout"
	gwmetrics "obsdatalayer/internal/metrics"
	"obsdatalayer/internal/middleware"
	"obsdatalayer/internal/proxy"
)

const (
	traceMethod   = "/opentelemetry.proto.collector.trace.v1.TraceService/Export"
	metricsMethod = "/opentelemetry.proto.collector.metrics.v1.MetricsService/Export"
	logsMethod    = "/opentelemetry.proto.collector.logs.v1.LogsService/Export"
)

const (
	upstreamErrorDetailBytes = 4096
)

var healthServices = []string{
	"",
	"opentelemetry.proto.collector.trace.v1.TraceService",
	"opentelemetry.proto.collector.metrics.v1.MetricsService",
	"opentelemetry.proto.collector.logs.v1.LogsService",
}

type targetDialer func(context.Context, config.PushTarget) (*grpc.ClientConn, error)

// Server owns the gRPC server and the upstream ClientConns it caches.
type Server struct {
	grpc     *grpc.Server
	receiver *Receiver
	health   *health.Server
}

// Serve serves the OTLP/gRPC listener.
func (s *Server) Serve(lis net.Listener) error {
	return s.grpc.Serve(lis)
}

// GracefulStop drains the listener and closes cached upstream connections.
func (s *Server) GracefulStop() {
	s.SetNotServing()
	s.grpc.GracefulStop()
	s.receiver.closeConns()
}

// Stop immediately stops the listener and closes cached upstream connections.
func (s *Server) Stop() {
	s.SetNotServing()
	s.grpc.Stop()
	s.receiver.closeConns()
}

// SetNotServing marks the gRPC health service unhealthy before the listener
// begins draining.
func (s *Server) SetNotServing() {
	if s == nil || s.health == nil {
		return
	}
	for _, service := range healthServices {
		s.health.SetServingStatus(service, healthpb.HealthCheckResponse_NOT_SERVING)
	}
}

// RetainTargets closes cached upstream connections no longer present in cfg.
func (s *Server) RetainTargets(cfg *config.Config) {
	if s == nil || s.receiver == nil {
		return
	}
	s.receiver.retainTargets(cfg)
}

// Receiver holds the shared dependencies for the three OTLP/gRPC export
// services.
type Receiver struct {
	holder *config.ConfigHolder
	auth   auth.Authorizer
	proxy  *proxy.Proxy
	m      *gwmetrics.Metrics
	guard  *middleware.AuthGuard

	dial   targetDialer
	connMu sync.Mutex
	conns  map[string]*grpc.ClientConn
}

// NewServer creates a gRPC server with OTLP trace, metrics and log services.
func NewServer(h *config.ConfigHolder, a auth.Authorizer, p *proxy.Proxy, m *gwmetrics.Metrics, guard *middleware.AuthGuard, opts ...grpc.ServerOption) *Server {
	return newServerWithDial(h, a, p, m, guard, dialTarget, opts...)
}

func newServerWithDial(h *config.ConfigHolder, a auth.Authorizer, p *proxy.Proxy, m *gwmetrics.Metrics, guard *middleware.AuthGuard, dial targetDialer, opts ...grpc.ServerOption) *Server {
	r := &Receiver{holder: h, auth: a, proxy: p, m: m, guard: guard, dial: dial, conns: map[string]*grpc.ClientConn{}}
	base := []grpc.ServerOption{grpc.UnaryInterceptor(r.unaryAuth), grpc.MaxRecvMsgSize(recvMsgSize(h))}
	opts = append(base, opts...)
	srv := grpc.NewServer(opts...)
	collecttrace.RegisterTraceServiceServer(srv, traceService{receiver: r})
	collectmetrics.RegisterMetricsServiceServer(srv, metricsService{receiver: r})
	collectlogs.RegisterLogsServiceServer(srv, logsService{receiver: r})
	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(srv, healthSrv)
	for _, service := range healthServices {
		healthSrv.SetServingStatus(service, healthpb.HealthCheckResponse_SERVING)
	}
	return &Server{grpc: srv, receiver: r, health: healthSrv}
}

func recvMsgSize(h *config.ConfigHolder) int {
	if h == nil {
		return 32 * 1024 * 1024
	}
	cfg := h.Get()
	if cfg == nil || cfg.Gateway.MaxBodyBytes <= 0 {
		return 32 * 1024 * 1024
	}
	if cfg.Gateway.MaxBodyBytes > int64(int(^uint(0)>>1)) {
		return int(^uint(0) >> 1)
	}
	return int(cfg.Gateway.MaxBodyBytes)
}

type traceService struct {
	collecttrace.UnimplementedTraceServiceServer
	receiver *Receiver
}

type metricsService struct {
	collectmetrics.UnimplementedMetricsServiceServer
	receiver *Receiver
}

type logsService struct {
	collectlogs.UnimplementedLogsServiceServer
	receiver *Receiver
}

// Export receives OTLP traces.
func (s traceService) Export(ctx context.Context, req *collecttrace.ExportTraceServiceRequest) (*collecttrace.ExportTraceServiceResponse, error) {
	resp, err := s.receiver.export(ctx, signalSpec{
		backend:     "tempo",
		httpPath:    "/v1/traces",
		httpGroup:   config.TargetGroupOTLPHTTP,
		newHTTPResp: func() proto.Message { return &collecttrace.ExportTraceServiceResponse{} },
		callGRPC: func(ctx context.Context, cc grpc.ClientConnInterface) (proto.Message, error) {
			return collecttrace.NewTraceServiceClient(cc).Export(ctx, req)
		},
		emptyResponse: &collecttrace.ExportTraceServiceResponse{},
	}, req)
	if err != nil {
		return nil, err
	}
	return resp.(*collecttrace.ExportTraceServiceResponse), nil
}

// Export receives OTLP metrics.
func (s metricsService) Export(ctx context.Context, req *collectmetrics.ExportMetricsServiceRequest) (*collectmetrics.ExportMetricsServiceResponse, error) {
	resp, err := s.receiver.export(ctx, signalSpec{
		backend:     "mimir",
		httpPath:    "/otlp/v1/metrics",
		httpGroup:   config.TargetGroupPush,
		newHTTPResp: func() proto.Message { return &collectmetrics.ExportMetricsServiceResponse{} },
		callGRPC: func(ctx context.Context, cc grpc.ClientConnInterface) (proto.Message, error) {
			return collectmetrics.NewMetricsServiceClient(cc).Export(ctx, req)
		},
		emptyResponse: &collectmetrics.ExportMetricsServiceResponse{},
	}, req)
	if err != nil {
		return nil, err
	}
	return resp.(*collectmetrics.ExportMetricsServiceResponse), nil
}

// Export receives OTLP logs.
func (s logsService) Export(ctx context.Context, req *collectlogs.ExportLogsServiceRequest) (*collectlogs.ExportLogsServiceResponse, error) {
	resp, err := s.receiver.export(ctx, signalSpec{
		backend:     "loki",
		httpPath:    "/otlp/v1/logs",
		httpGroup:   config.TargetGroupPush,
		newHTTPResp: func() proto.Message { return &collectlogs.ExportLogsServiceResponse{} },
		callGRPC: func(ctx context.Context, cc grpc.ClientConnInterface) (proto.Message, error) {
			return collectlogs.NewLogsServiceClient(cc).Export(ctx, req)
		},
		emptyResponse: &collectlogs.ExportLogsServiceResponse{},
	}, req)
	if err != nil {
		return nil, err
	}
	return resp.(*collectlogs.ExportLogsServiceResponse), nil
}

type signalSpec struct {
	backend       string
	httpPath      string
	httpGroup     string
	newHTTPResp   func() proto.Message
	callGRPC      func(context.Context, grpc.ClientConnInterface) (proto.Message, error)
	emptyResponse proto.Message
}

func (r *Receiver) export(ctx context.Context, spec signalSpec, req proto.Message) (proto.Message, error) {
	ra := auth.FromContext(ctx)
	cfg := r.holder.Get()
	if cfg == nil {
		return nil, status.Error(codes.Unavailable, "no configuration loaded")
	}
	if cfg.Gateway.MaxBodyBytes > 0 && int64(proto.Size(req)) > cfg.Gateway.MaxBodyBytes {
		return nil, status.Error(codes.ResourceExhausted, "request body too large")
	}
	inst, err := fanout.SelectInstance(cfg, ra, spec.backend)
	if err != nil {
		if errors.Is(err, fanout.ErrAmbiguousInstance) {
			return nil, status.Error(codes.FailedPrecondition, fanout.ErrAmbiguousInstance.Error())
		}
		return nil, status.Error(codes.NotFound, "no matching instance")
	}
	if err := fanout.ScopeAuthToInstance(ra, inst); err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}

	if targets := inst.GetExactTargets(config.TargetGroupOTLPGRPC); len(targets) > 0 {
		return r.exportGRPC(ctx, inst, targets, spec)
	}
	return r.exportHTTP(ctx, inst, spec, req)
}

type grpcTargetResult struct {
	target config.PushTarget
	resp   proto.Message
	err    error
}

func (r *Receiver) exportGRPC(ctx context.Context, inst *config.InstanceConfig, targets []config.PushTarget, spec signalSpec) (proto.Message, error) {
	results := make([]grpcTargetResult, len(targets))
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Add(1)
		go func(idx int, tgt config.PushTarget) {
			defer wg.Done()
			callCtx, cancel := context.WithTimeout(ctx, tgt.Timeout(r.proxy.DefaultTargetTimeout()))
			defer cancel()
			cc, err := r.connFor(callCtx, tgt)
			if err != nil {
				results[idx] = grpcTargetResult{target: tgt, err: err}
				return
			}
			resp, err := spec.callGRPC(outgoingContext(callCtx, tgt), cc)
			results[idx] = grpcTargetResult{target: tgt, resp: resp, err: err}
		}(i, target)
	}
	wg.Wait()

	for _, result := range results {
		r.recordGRPCFanout(inst, result)
	}

	mode := inst.FanOutMode
	if mode == "" {
		mode = "any"
	}
	if mode == "all" {
		for _, result := range results {
			if result.err != nil {
				return nil, upstreamGRPCError(result.err)
			}
		}
		return firstGRPCResponse(results, spec.emptyResponse), nil
	}

	var firstSuccess proto.Message
	var firstErr error
	var partialFailures []fanout.PartialFailure
	for _, result := range results {
		if result.err == nil {
			if firstSuccess == nil {
				firstSuccess = nonNilResponse(result.resp, spec.emptyResponse)
			}
			continue
		}
		partialFailures = append(partialFailures, fanout.PartialFailure{
			Instance:   inst.Name,
			StatusCode: httpStatusFromGRPCError(result.err),
		})
		if firstErr == nil {
			firstErr = result.err
		}
	}
	if firstSuccess != nil {
		if len(partialFailures) > 0 {
			if r.m != nil {
				r.m.RecordPartialFailure(inst.Name)
			}
			_ = grpc.SetTrailer(ctx, metadata.Pairs("x-gateway-partial-failure", fanout.FormatPartialFailureHeader(partialFailures)))
		}
		return firstSuccess, nil
	}
	return nil, upstreamGRPCError(firstErr)
}

func (r *Receiver) exportHTTP(ctx context.Context, inst *config.InstanceConfig, spec signalSpec, req proto.Message) (proto.Message, error) {
	body, err := proto.Marshal(req)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal OTLP request: %v", err)
	}
	targets := inst.GetTargets(spec.httpGroup)
	if len(targets) == 0 {
		return nil, status.Errorf(codes.Internal, "instance has no target for group %s", spec.httpGroup)
	}
	statusCode, respBody, respHeaders, partialFailures := fanout.Do(ctx, inst, targets, body, otlpHTTPHeaders(ctx), spec.httpPath, r.proxy.PushClient(), r.m)
	if len(partialFailures) > 0 && r.m != nil {
		r.m.RecordPartialFailure(inst.Name)
	}
	if statusCode < 200 || statusCode >= 300 {
		return nil, httpStatusError(statusCode, respBody, respHeaders)
	}
	if len(respBody) == 0 {
		return spec.emptyResponse, nil
	}
	resp := spec.newHTTPResp()
	if err := proto.Unmarshal(respBody, resp); err != nil {
		return nil, status.Errorf(codes.Internal, "decode OTLP HTTP response: %v", err)
	}
	return resp, nil
}

func firstGRPCResponse(results []grpcTargetResult, fallback proto.Message) proto.Message {
	for _, result := range results {
		if result.err == nil && result.resp != nil {
			return result.resp
		}
	}
	return fallback
}

func nonNilResponse(resp, fallback proto.Message) proto.Message {
	if resp != nil {
		return resp
	}
	return fallback
}

func (r *Receiver) recordGRPCFanout(inst *config.InstanceConfig, result grpcTargetResult) {
	if r.m == nil {
		return
	}
	if result.err != nil {
		r.m.RecordFanout(inst.Name, result.target.URL, httpStatusFromGRPCError(result.err))
		return
	}
	r.m.RecordFanout(inst.Name, result.target.URL, http.StatusOK)
}

func (r *Receiver) connFor(ctx context.Context, target config.PushTarget) (*grpc.ClientConn, error) {
	key := targetConnKey(target)
	r.connMu.Lock()
	cc := r.conns[key]
	r.connMu.Unlock()
	if cc != nil {
		return cc, nil
	}
	cc, err := r.dial(ctx, target)
	if err != nil {
		return nil, err
	}
	r.connMu.Lock()
	if existing := r.conns[key]; existing != nil {
		r.connMu.Unlock()
		_ = cc.Close()
		return existing, nil
	}
	r.conns[key] = cc
	r.connMu.Unlock()
	return cc, nil
}

func (r *Receiver) retainTargets(cfg *config.Config) {
	live := map[string]struct{}{}
	if cfg != nil {
		for _, inst := range cfg.Instances {
			for _, target := range inst.GetExactTargets(config.TargetGroupOTLPGRPC) {
				live[targetConnKey(target)] = struct{}{}
			}
		}
	}
	var close []*grpc.ClientConn
	r.connMu.Lock()
	for key, cc := range r.conns {
		if _, ok := live[key]; ok {
			continue
		}
		close = append(close, cc)
		delete(r.conns, key)
	}
	r.connMu.Unlock()
	for _, cc := range close {
		_ = cc.Close()
	}
}

func (r *Receiver) closeConns() {
	r.retainTargets(nil)
}

func targetConnKey(target config.PushTarget) string {
	return target.URL + "\x00" + strconv.FormatBool(target.SkipTLSVerify)
}

func dialTarget(_ context.Context, target config.PushTarget) (*grpc.ClientConn, error) {
	u, err := url.Parse(target.URL)
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, fmt.Errorf("target %q has no host", target.URL)
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("gRPC target %q must not include a path", target.URL)
	}
	var transport credentials.TransportCredentials
	switch u.Scheme {
	case "http":
		transport = insecure.NewCredentials()
	case "https":
		cfg := &tls.Config{ServerName: serverName(u.Host)}
		if target.SkipTLSVerify {
			cfg.InsecureSkipVerify = true
		}
		transport = credentials.NewTLS(cfg)
	default:
		return nil, fmt.Errorf("unsupported gRPC target scheme %q", u.Scheme)
	}
	return grpc.NewClient(u.Host, grpc.WithTransportCredentials(transport))
}

func serverName(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err == nil {
		return host
	}
	return hostport
}

func outgoingContext(ctx context.Context, target config.PushTarget) context.Context {
	md := metadata.MD{}
	if target.BasicAuth != "" {
		md.Set("authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(target.BasicAuth)))
	}
	if orgID := orgIDFromContext(ctx, target); orgID != "" {
		md.Set("x-scope-orgid", orgID)
	}
	if inbound, ok := metadata.FromIncomingContext(ctx); ok {
		for _, key := range []string{"traceparent", "tracestate"} {
			if vals := inbound.Get(key); len(vals) > 0 {
				md.Set(key, vals...)
			}
		}
	}
	return metadata.NewOutgoingContext(ctx, md)
}

func orgIDFromContext(ctx context.Context, target config.PushTarget) string {
	ra := auth.FromContext(ctx)
	if ra != nil && len(ra.TenantIDs) > 0 {
		if target.TenantID != "" {
			return target.TenantID
		}
		if ra.IsRead {
			return strings.Join(ra.TenantIDs, "|")
		}
		return ra.TenantIDs[0]
	}
	return target.TenantID
}

func otlpHTTPHeaders(ctx context.Context) http.Header {
	h := http.Header{"Content-Type": []string{"application/x-protobuf"}}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		for _, key := range []string{"traceparent", "tracestate"} {
			if vals := md.Get(key); len(vals) > 0 {
				h[http.CanonicalHeaderKey(key)] = vals
			}
		}
	}
	return h
}

func upstreamGRPCError(err error) error {
	if err == nil {
		return status.Error(codes.Unavailable, "upstream unavailable")
	}
	if s, ok := status.FromError(err); ok {
		return status.Error(s.Code(), "upstream "+s.Message())
	}
	return status.Error(codes.Unavailable, "upstream unavailable")
}

func httpStatusError(statusCode int, body []byte, headers http.Header) error {
	msg := http.StatusText(statusCode)
	if msg == "" {
		msg = "upstream HTTP error"
	}
	if len(body) > 0 {
		detailBody := body
		if len(detailBody) > upstreamErrorDetailBytes {
			detailBody = detailBody[:upstreamErrorDetailBytes]
		}
		detail := strings.TrimSpace(strings.ToValidUTF8(string(detailBody), ""))
		if detail != "" {
			msg = msg + ": " + detail
		}
	}
	switch statusCode {
	case http.StatusBadRequest:
		return status.Error(codes.InvalidArgument, msg)
	case http.StatusUnauthorized:
		return status.Error(codes.Unauthenticated, msg)
	case http.StatusForbidden:
		return status.Error(codes.PermissionDenied, msg)
	case http.StatusNotFound:
		return status.Error(codes.NotFound, msg)
	case http.StatusRequestEntityTooLarge:
		return status.Error(codes.ResourceExhausted, msg)
	case http.StatusTooManyRequests:
		if retryAfter := parseRetryAfter(headers.Get("Retry-After")); retryAfter > 0 {
			return statusWithRetryInfo(codes.ResourceExhausted, msg, retryAfter)
		}
		return status.Error(codes.Unavailable, msg)
	default:
		if statusCode >= 500 {
			if retryAfter := parseRetryAfter(headers.Get("Retry-After")); retryAfter > 0 {
				return statusWithRetryInfo(codes.Unavailable, msg, retryAfter)
			}
			return status.Error(codes.Unavailable, msg)
		}
		return status.Error(codes.Unknown, msg)
	}
}

func httpStatusFromGRPCError(err error) int {
	if err == nil {
		return http.StatusOK
	}
	s, ok := status.FromError(err)
	if !ok {
		return 0
	}
	switch s.Code() {
	case codes.OK:
		return http.StatusOK
	case codes.Canceled:
		return 499
	case codes.InvalidArgument:
		return http.StatusBadRequest
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists, codes.Aborted:
		return http.StatusConflict
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests
	case codes.FailedPrecondition, codes.OutOfRange:
		return http.StatusPreconditionFailed
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

type methodSpec struct {
	backend string
	action  string
}

func specForMethod(method string) (methodSpec, bool) {
	switch method {
	case traceMethod:
		return methodSpec{backend: "tempo", action: auth.ActionWrite}, true
	case metricsMethod:
		return methodSpec{backend: "mimir", action: auth.ActionWrite}, true
	case logsMethod:
		return methodSpec{backend: "loki", action: auth.ActionWrite}, true
	default:
		return methodSpec{}, false
	}
}

func (r *Receiver) unaryAuth(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	spec, ok := specForMethod(info.FullMethod)
	if !ok {
		return handler(ctx, req)
	}
	started := time.Now()
	source := sourceKey(ctx)
	defer func() {
		slog.Debug("otlp grpc request complete",
			"method", info.FullMethod,
			"backend", spec.backend,
			"code", status.Code(err).String(),
			"duration", time.Since(started),
			"source", source)
	}()

	if r.guard != nil && r.guard.Limiter != nil {
		allowed, retryAfter := r.guard.Limiter.Allow(source)
		if !allowed {
			slog.Warn("authentication throttled",
				"plane", "otlp_grpc", "source", source, "retry_after", retryAfter, "method", info.FullMethod)
			if r.guard.Metrics != nil {
				r.guard.Metrics.RecordAuthRejected("throttled")
			}
			return nil, retryLaterError("too many failed authentication attempts", retryAfter)
		}
	}

	username, err := r.authenticate(ctx)
	if err != nil {
		r.recordAuthFailure(source)
		return nil, err
	}
	r.recordAuthSuccess(source)

	decision := r.auth.AccessDecision(username, spec.backend, spec.action)
	if !decision.Allowed {
		slog.Info("data plane request denied",
			"status", codes.PermissionDenied.String(),
			"phase", "authorization",
			"reason", decision.DenyReason,
			"user", username,
			"backend", spec.backend,
			"action", spec.action,
			"path", info.FullMethod,
			"tenant_count", decision.TenantCount)
		return nil, status.Error(codes.PermissionDenied, "forbidden")
	}
	ra := &auth.RequestAuth{
		Username:       username,
		TenantIDs:      decision.Access.TenantIDs,
		LabelSelectors: decision.Access.LabelSelectors,
		IsRead:         false,
	}
	return handler(auth.WithRequestAuth(ctx, ra), req)
}

func retryLaterError(msg string, retryAfter time.Duration) error {
	return statusWithRetryInfo(codes.ResourceExhausted, msg, retryAfter)
}

func statusWithRetryInfo(code codes.Code, msg string, retryAfter time.Duration) error {
	st := status.New(code, msg)
	if retryAfter <= 0 {
		return st.Err()
	}
	withDetails, err := st.WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(retryAfter)})
	if err != nil {
		return st.Err()
	}
	return withDetails.Err()
}

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	at, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	d := time.Until(at)
	if d < 0 {
		return 0
	}
	return d
}

func (r *Receiver) authenticate(ctx context.Context) (string, error) {
	if r.auth == nil {
		return "", status.Error(codes.Unauthenticated, "authentication unavailable")
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "missing credentials")
	}
	authz := firstMetadataValue(md, "authorization")
	if token, isBearer := auth.BearerToken(authz); isBearer {
		u, err := r.auth.AuthenticateAPIKey(token)
		if err != nil {
			return "", status.Error(codes.Unauthenticated, "invalid credentials")
		}
		return u.Name, nil
	}
	username, password, ok := parseBasic(authz)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "missing credentials")
	}
	if _, err := r.auth.AuthenticateContext(ctx, username, password); err != nil {
		if errors.Is(err, auth.ErrHashLimitReached) {
			if r.guard != nil && r.guard.Metrics != nil {
				r.guard.Metrics.RecordAuthRejected("saturated")
			}
			return "", status.Error(codes.Unavailable, "authentication capacity reached, retry shortly")
		}
		return "", status.Error(codes.Unauthenticated, "invalid credentials")
	}
	return username, nil
}

func firstMetadataValue(md metadata.MD, key string) string {
	vals := md.Get(key)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

func parseBasic(value string) (string, string, bool) {
	if !strings.HasPrefix(strings.ToLower(value), "basic ") {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value[6:]))
	if err != nil {
		return "", "", false
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	return user, pass, ok
}

func (r *Receiver) recordAuthFailure(source string) {
	if r.guard == nil {
		return
	}
	if r.guard.Metrics != nil {
		r.guard.Metrics.RecordAuthFailure()
	}
	if r.guard.Limiter != nil {
		r.guard.Limiter.RecordFailure(source)
	}
}

func (r *Receiver) recordAuthSuccess(source string) {
	if r.guard == nil || r.guard.Limiter == nil {
		return
	}
	r.guard.Limiter.RecordSuccess(source)
}

func sourceKey(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return authlimit.SourceKey(p.Addr.String())
	}
	return ""
}
