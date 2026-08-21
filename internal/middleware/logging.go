package middleware

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Hijack passes the connection takeover through to the underlying writer.
// Without it a protocol upgrade fails with "can't switch protocols using
// non-Hijacker ResponseWriter", because wrapping to capture the status code
// hides the capability the upgrade needs. Loki's live tail is a WebSocket and
// depends on this.
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("logging: ResponseWriter of type %T does not support hijacking", rw.ResponseWriter)
	}
	// A hijacked connection writes its own status line, so record the upgrade
	// rather than leaving the default 200 in the access log.
	rw.statusCode = http.StatusSwitchingProtocols
	return hj.Hijack()
}

// Flush passes streaming flushes through, so a chunked upstream response is not
// buffered until the handler returns.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Logging wraps a handler with structured request/response logging using log/slog.
//
// The line is debug rather than info. One line per request is the largest
// single source of log volume the gateway produces, and on a hosted backend
// that volume is billed. What an operator needs at the default level is logged
// where it happens instead: authorization denials, throttling, and non-2xx
// upstream responses each have their own line outside this one.
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r)
		slog.Debug("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.statusCode,
			"duration", time.Since(start).String(),
		)
	})
}
