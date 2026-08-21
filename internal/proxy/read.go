package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"obsdatalayer/internal/config"
)

// maxReplayableReadBody caps how much of a read request's body is buffered so
// it can be replayed against another target.
//
// Reads carry a body only on the form-encoded query endpoints, where it holds a
// LogQL or PromQL expression and a time range. A megabyte is far more than any
// real query needs. A body larger than this is streamed straight through to a
// single target instead, which loses failover for that request rather than
// holding an arbitrary amount of memory to preserve it.
const maxReplayableReadBody = 1 << 20

// readAttempt is one upstream read, held back from the client until it is known
// whether the next target should be tried instead.
type readAttempt struct {
	url      string
	duration time.Duration

	// target is the upstream base URL, without the path or query. It is what
	// the counters are labeled by: a.url carries the caller's query string and
	// would give every distinct query its own series.
	target string

	// resp is non-nil when the upstream answered. Its body is still open and
	// must be either committed or discarded.
	resp *http.Response

	// body is the response body already read, for a status this attempt has
	// decided not to stream. Set for 5xx, where the body is needed for logging.
	body []byte

	// err is the transport failure, when the upstream did not answer at all.
	err error

	// retryable reports whether another target should be tried.
	retryable bool

	// cancel releases the attempt's per-target timeout context. It must not run
	// until the response body has been committed or discarded: cancelling it
	// kills the connection under an unread body, so an attempt that streams
	// straight to the client owns its cancel for as long as the body is open.
	cancel context.CancelFunc
}

// release drops the attempt's timeout context. Safe to call more than once.
func (a *readAttempt) release() {
	if a != nil && a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
}

// ForwardQuery forwards a read, trying each target in turn until one answers.
//
// A fan-out instance pushes to every target, so the targets are replicas and
// any of them can serve a query. Trying them in order means one target being
// down degrades read latency rather than failing reads outright, which is what
// happened when every read went to the first push target unconditionally.
//
// Only transport failures and 5xx move on to the next target. A 4xx is the
// upstream answering: asking a replica the same malformed question returns the
// same answer while doubling the work, and a 404 from a query endpoint is a
// legitimate result, not an outage.
func (p *Proxy) ForwardQuery(w http.ResponseWriter, r *http.Request, inst *config.InstanceConfig, upstreamPath string) {
	targets := inst.GetReadTargets()
	if len(targets) == 0 {
		WriteJSONError(w, http.StatusInternalServerError, map[string]string{
			"error": "instance has no read target", "instance": inst.Name,
		})
		return
	}
	// Read attempts use a client with no whole-call timeout: each attempt is
	// bounded by its own target's allowance instead.
	client := p.ReadClient()

	body, replayable, err := readReplayableBody(r)
	if err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			WriteJSONError(w, http.StatusRequestEntityTooLarge, map[string]string{
				"error": "request body too large", "instance": inst.Name,
			})
			return
		}
		WriteJSONError(w, http.StatusBadRequest, map[string]string{
			"error": "failed to read request body", "instance": inst.Name,
		})
		return
	}

	// Two separate bounds, deliberately.
	//
	// Each attempt is bounded by its own target's timeout, falling back to the
	// gateway's default_target_timeout. The allowance belongs to the target
	// because targets are independent systems: a local cluster and a remote DR
	// site behind one instance do not answer at the same speed. Dividing a
	// single budget between them was tried and is wrong -- it made each
	// target's allowance depend on how many replicas happened to be
	// configured, so adding a third silently shortened the first two.
	//
	// The whole read is bounded by the request context, which is cancelled when
	// the caller disconnects. The caller decides how long it is willing to
	// wait, and when it stops waiting the gateway stops working on its behalf,
	// abandoning the attempt in flight.
	ctx := r.Context()
	defaultTimeout := p.DefaultTargetTimeout()

	ordered := p.health.order(targets)
	if !replayable {
		// The body was too large to buffer, so it can only be sent once.
		ordered = ordered[:1]
	}

	// Each attempt needs its own reader over the buffered body. When the body
	// could not be buffered there is only one attempt, and it streams directly.
	bodyFor := func() io.Reader {
		if !replayable {
			return r.Body
		}
		if len(body) == 0 {
			return nil
		}
		return bytes.NewReader(body)
	}

	var last *readAttempt
	for i, target := range ordered {
		if last != nil {
			last.discard()
		}
		attempt := p.attemptRead(ctx, target.Timeout(defaultTimeout), r, bodyFor, inst, target, upstreamPath, client)
		if !attempt.retryable {
			p.health.recordSuccess(target.URL)
			p.recordRead(inst.Name, target.URL, true)
			if i > 0 {
				// The read was served, but only after something failed. Counted
				// separately so a dashboard can distinguish "healthy" from
				// "working because a replica covered for a broken one".
				p.recordReadFailover(inst.Name)
			}
			attempt.commit(p, w, r, inst)
			return
		}

		p.health.recordFailure(target.URL)
		p.recordRead(inst.Name, target.URL, false)
		last = attempt
		if i < len(ordered)-1 {
			slog.Warn("read target failed, trying the next",
				"instance", inst.Name, "target", target.URL,
				"status", attempt.status(), "error", attempt.err,
				"duration", attempt.duration)
		}
	}

	// Every target failed. Report the last failure rather than inventing one,
	// so the client sees a real upstream status where there was one.
	slog.Warn("every read target failed",
		"instance", inst.Name, "targets", len(ordered), "last_target", last.url)
	if len(ordered) > 1 {
		p.recordReadFailover(inst.Name)
	}
	last.commit(p, w, r, inst)
}

// attemptRead performs one upstream read and classifies the outcome without
// touching the client's ResponseWriter.
func (p *Proxy) attemptRead(
	ctx context.Context,
	timeout time.Duration,
	r *http.Request,
	bodyFor func() io.Reader,
	inst *config.InstanceConfig,
	target config.PushTarget,
	upstreamPath string,
	client *http.Client,
) *readAttempt {

	upstreamURL := target.URL + upstreamPath
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	attemptCtx := ctx
	cancel := context.CancelFunc(func() {})
	if timeout > 0 {
		attemptCtx, cancel = context.WithTimeout(attemptCtx, timeout)
	}
	if target.SkipTLSVerify {
		attemptCtx = WithSkipTLSVerify(attemptCtx)
	}

	req, err := http.NewRequestWithContext(attemptCtx, r.Method, upstreamURL, bodyFor())
	if err != nil {
		// A malformed target URL is a configuration fault, not an outage, and
		// the next target would be built the same way. Do not retry it.
		cancel()
		return &readAttempt{url: upstreamURL, target: target.URL, err: err}
	}
	CopyHeadersForUpstream(req, r.Header, target)

	slog.Debug("forwarding upstream",
		"instance", inst.Name, "method", r.Method, "url", upstreamURL,
		"timeout", timeout, "org_id", req.Header.Get("X-Scope-OrgID"))

	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		slog.Debug("upstream request failed",
			"instance", inst.Name, "url", upstreamURL,
			"duration", time.Since(started), "error", err)
		cancel()
		return &readAttempt{
			url: upstreamURL, target: target.URL, err: err, duration: time.Since(started),
			// A target timing out is exactly the case worth retrying: it hung,
			// and a replica may answer. The caller's context is what says to
			// stop -- once it is cancelled nobody is waiting for the answer.
			// A body limit breach is the caller's fault and identical at every
			// target.
			retryable: !isBodyLimitError(err) && ctx.Err() == nil,
		}
	}

	slog.Debug("upstream responded",
		"instance", inst.Name, "url", upstreamURL,
		"status", resp.StatusCode, "duration", time.Since(started))

	attempt := &readAttempt{
		url: upstreamURL, target: target.URL, resp: resp,
		duration: time.Since(started), cancel: cancel,
	}
	if resp.StatusCode >= 500 {
		// Read the body now: it is needed for the log line, and holding the
		// connection open across another attempt would pin it for nothing.
		payload, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		attempt.body = payload
		attempt.retryable = true
		LogUpstreamNon2XX(inst.Name, r.Method, upstreamURL, resp.StatusCode,
			attempt.duration, req.Header.Get("X-Scope-OrgID"), payload, readErr)
	}
	return attempt
}

func (a *readAttempt) status() int {
	if a.resp == nil {
		return 0
	}
	return a.resp.StatusCode
}

// discard releases an attempt that will not be sent to the client.
func (a *readAttempt) discard() {
	if a == nil {
		return
	}
	defer a.release()
	if a.resp == nil || a.body != nil {
		return
	}
	_, _ = io.Copy(io.Discard, a.resp.Body)
	_ = a.resp.Body.Close()
}

// commit writes the attempt to the client. It is called exactly once per
// request, on the attempt that decided the outcome.
//
// It takes the Proxy only to reach the counters: a body that fails part-way is
// invisible in the response, because the status line has already gone out, so
// the only place it can be reported is a log line and a counter.
func (a *readAttempt) commit(p *Proxy, w http.ResponseWriter, r *http.Request, inst *config.InstanceConfig) {
	// The timeout context stays live until the body has been copied out, and
	// is released here rather than when the attempt was made.
	defer a.release()
	if a.resp == nil {
		writeTransportError(w, inst, a.err)
		return
	}

	for key, vals := range a.resp.Header {
		for _, v := range vals {
			w.Header().Add(key, v)
		}
	}

	// A body already read is written from the buffer; anything else is streamed
	// so a large query result is not held in memory.
	if a.body != nil {
		w.WriteHeader(a.resp.StatusCode)
		if len(a.body) > 0 {
			_, _ = w.Write(a.body)
		}
		return
	}
	defer a.resp.Body.Close()

	if isNon2XX(a.resp.StatusCode) {
		payload, readErr := io.ReadAll(a.resp.Body)
		LogUpstreamNon2XX(inst.Name, r.Method, a.url, a.resp.StatusCode,
			a.duration, "", payload, readErr)
		w.WriteHeader(a.resp.StatusCode)
		if len(payload) > 0 {
			_, _ = w.Write(payload)
		}
		return
	}
	w.WriteHeader(a.resp.StatusCode)
	written, err := io.Copy(w, a.resp.Body)
	if err == nil {
		return
	}

	// A body that stops part-way is deliberately NOT handled like a failed
	// attempt, and must not reach the failover path:
	//
	//   - There is nothing to retry. The status line and headers are already on
	//     the wire, so a replica's answer could not be sent even if one were
	//     asked for it.
	//   - The target is not unwell. It answered, and the copy can fail on the
	//     client's side of the gateway just as easily as the upstream's.
	//     Recording a target failure here would park a working replica in the
	//     read cool-off over a request it served correctly.
	//
	// Which side gave up decides how it is reported. The whole read is bound to
	// the request context, which net/http cancels when the caller disconnects,
	// so a cancelled query -- a dashboard panel closed, a browser tab shut --
	// arrives here as a copy error like any other. That is routine and nobody's
	// fault: counting it as a truncation would bury the case below in a stream
	// of ordinary client behaviour and make an alert on it useless.
	if clientGone(r, err) {
		slog.Debug("read abandoned; the client disconnected before the body finished",
			"instance", inst.Name, "target", a.target, "method", r.Method, "url", a.url,
			"status", a.resp.StatusCode, "bytes_written", written, "error", err)
		p.recordReadClientDisconnect(inst.Name, a.target)
		// Nothing is listening for a terminator, so there is no framing left to
		// get right. Abort anyway rather than return: it releases the
		// connection immediately instead of writing a trailer to a dead socket.
		panic(http.ErrAbortHandler)
	}

	// The client is still there, so what it must not be left with is a
	// well-formed short body. The upstream framing decides how bad that is, and
	// the quiet case is the common one: when the upstream answered chunked,
	// this response is chunked too, and returning normally would have Go write
	// the terminating chunk -- so the caller reads a complete-looking reply
	// that is simply missing data. For a JSON API like the Prometheus query
	// endpoints that surfaces as an inexplicable parse error, with the gateway
	// reporting 200.
	//
	// Aborting the handler drops the connection without a terminator instead,
	// which every HTTP client reports as a failed read. The caller learns the
	// answer was incomplete, which is the one thing it cannot work out for
	// itself, and the log line and counter say why.
	slog.Error("read response body truncated; aborting the connection so the client cannot mistake it for a complete answer",
		"instance", inst.Name, "target", a.target, "method", r.Method, "url", a.url,
		"status", a.resp.StatusCode, "bytes_written", written,
		"content_length", a.resp.ContentLength, "error", err)
	p.recordReadTruncated(inst.Name, a.target)

	// Recovered by net/http, which closes the connection and logs nothing
	// further. The deferred release above still runs.
	panic(http.ErrAbortHandler)
}

// clientGone reports whether a body copy stopped because the caller went away
// rather than because the upstream cut the answer short.
//
// The request context is the reliable signal: net/http cancels it when the
// client disconnects, and the upstream request was made under it, so both the
// read from the upstream and the write to the client fail with that
// cancellation. A context that is still live means the client is still waiting,
// and the short body is the upstream's doing.
func clientGone(r *http.Request, err error) bool {
	return r.Context().Err() != nil || errors.Is(err, context.Canceled)
}

// writeTransportError maps a failure to reach an upstream onto a client status.
func writeTransportError(w http.ResponseWriter, inst *config.InstanceConfig, err error) {
	switch {
	case isBodyLimitError(err):
		WriteJSONError(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": "request body too large", "instance": inst.Name,
		})
	case isTimeoutError(err):
		WriteJSONError(w, http.StatusGatewayTimeout, map[string]string{
			"error": "upstream timeout", "instance": inst.Name,
		})
	default:
		WriteJSONError(w, http.StatusBadGateway, map[string]string{
			"error": "upstream unavailable", "instance": inst.Name,
		})
	}
}

func isBodyLimitError(err error) bool {
	var maxBytes *http.MaxBytesError
	return errors.As(err, &maxBytes)
}

// readReplayableBody buffers a read request's body so it can be sent to more
// than one target. It reports whether the body fits the replay cap; a body that
// does not is left on the request to be streamed to a single target.
func readReplayableBody(r *http.Request) (body []byte, replayable bool, err error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, true, nil
	}

	limited := io.LimitReader(r.Body, maxReplayableReadBody+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return nil, false, err
	}
	if len(buf) > maxReplayableReadBody {
		// Too big to hold. Put what was read back in front of the rest so the
		// single attempt still sends the whole body.
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(buf), r.Body), r.Body}
		return nil, false, nil
	}
	return buf, true, nil
}
