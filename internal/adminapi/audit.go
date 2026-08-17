package adminapi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"obsdatalayer/internal/auth"
)

// sensitiveKeys never appear in a log line. Values under these keys are masked
// before the request body is recorded, so an audit trail can show what was
// submitted without also capturing credentials.
var sensitiveKeys = map[string]bool{
	"password":        true,
	"basic_auth":      true,
	"password_bcrypt": true,
	"credential":      true,
	"dsn":             true,
}

// audited wraps a state-changing handler with a start and finish log line.
//
// The pair exists so an operator can see both what was asked for and what came
// of it: the started line records the actor and the submitted document, and the
// finished line records the status and how long it took. Reads are not wrapped;
// only mutations are worth the noise.
func (h *handler) audited(op string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor := "anonymous"
		if ra := auth.FromContext(r.Context()); ra != nil && ra.Username != "" {
			actor = ra.Username
		}
		target := r.PathValue("name")
		if target == "" {
			target = r.PathValue("id")
		}

		// Buffer the body so it can be logged and still decoded downstream.
		// One byte over the cap is read so the handler's MaxBytesReader still
		// trips on an oversized document rather than seeing it truncated.
		var body []byte
		if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodDelete {
			body, _ = io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
			r.Body = io.NopCloser(bytes.NewReader(body))
		}

		// On a create the subject is in the body rather than the path, so the
		// started line would otherwise not say what was being created.
		if target == "" {
			target = subjectFromBody(body)
		}

		slog.Info("admin operation started",
			"op", op, "actor", actor, "target", target, "method", r.Method, "path", r.URL.Path)
		if len(body) > 0 {
			slog.Debug("admin operation request body",
				"op", op, "actor", actor, "body", redactJSON(body))
		}

		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next(rec, r)
		elapsed := time.Since(start)

		outcome, level := "completed", slog.LevelInfo
		if rec.status >= http.StatusBadRequest {
			outcome, level = "failed", slog.LevelWarn
		}
		slog.Log(r.Context(), level, "admin operation "+outcome,
			"op", op, "actor", actor, "target", target,
			"status", rec.status, "duration", elapsed)
	}
}

// statusRecorder captures the response status so the finish line can report the
// outcome without the handlers having to cooperate.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.status = code
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}

// subjectFromBody pulls the human-meaningful identifier out of a submitted
// document, so a create can be audited by name rather than as an empty target.
func subjectFromBody(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	for _, key := range []string{"name", "id"} {
		if v, ok := doc[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// redactJSON renders a request body with sensitive values masked. Anything that
// does not parse as JSON is reported as such rather than echoed, so a malformed
// body cannot smuggle raw credentials into the log.
func redactJSON(raw []byte) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "<unparsable body omitted>"
	}
	out, err := json.Marshal(redactValue(v))
	if err != nil {
		return "<unencodable body omitted>"
	}
	return string(out)
}

func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if sensitiveKeys[k] {
				t[k] = redacted
				continue
			}
			t[k] = redactValue(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = redactValue(val)
		}
		return t
	default:
		return v
	}
}
