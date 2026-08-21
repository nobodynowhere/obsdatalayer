package rewrite

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"obsdatalayer/internal/auth"
)

// ApplyLokiReadPolicy constrains Loki read request parameters with the resolved
// label selector policy carried by the auth middleware.
func ApplyLokiReadPolicy(r *http.Request, endpoint string) error {
	ra := auth.FromContext(r.Context())
	if ra == nil || len(ra.LabelSelectors) == 0 {
		return nil
	}

	values, commit, err := mutableRequestValues(r)
	if err != nil {
		return err
	}

	switch endpoint {
	case "query", "query_range", "tail":
		query := strings.TrimSpace(values.Get("query"))
		if query == "" {
			return errors.New("query parameter is required for a restricted Loki read")
		}
		rewritten, err := ConstrainLogQL(query, ra.LabelSelectors)
		if err != nil {
			return err
		}
		values.Set("query", rewritten)
	case "labels", "label_values", "index_stats", "index_volume", "index_volume_range", "patterns", "detected_fields", "detected_field_values":
		if err := constrainOptionalLokiQuery(values, ra.LabelSelectors); err != nil {
			return err
		}
	case "series":
		if err := ConstrainMetricSelectorParams(values, ra.LabelSelectors); err != nil {
			return err
		}
	case "format_query":
		query := strings.TrimSpace(values.Get("query"))
		if query != "" {
			rewritten, err := ConstrainLogQL(query, ra.LabelSelectors)
			if err != nil {
				return err
			}
			values.Set("query", rewritten)
		}
	case "status_buildinfo":
		// Build info is not a data read.
	default:
		return fmt.Errorf("unknown Loki read endpoint %q", endpoint)
	}

	commit(values)
	return nil
}

func constrainOptionalLokiQuery(values map[string][]string, selectors []string) error {
	policySelector, _, err := singlePolicySelector(selectors)
	if err != nil {
		return err
	}
	query := strings.TrimSpace(firstValue(values, "query"))
	if query == "" {
		values["query"] = []string{policySelector}
		return nil
	}
	rewritten, err := ConstrainLogQL(query, selectors)
	if err != nil {
		return err
	}
	values["query"] = []string{rewritten}
	return nil
}

func firstValue(values map[string][]string, key string) string {
	if got := values[key]; len(got) > 0 {
		return got[0]
	}
	return ""
}

// ConstrainLogQL intersects every LogQL stream selector in query with the
// policy selector. It deliberately recognizes only brace-delimited label
// selectors outside strings and validates each one with the same matcher parser
// used for PromQL selectors.
func ConstrainLogQL(query string, selectors []string) (string, error) {
	policySelector, _, err := singlePolicySelector(selectors)
	if err != nil {
		return "", err
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return "", errors.New("LogQL query must not be empty for a restricted Loki read")
	}

	var out strings.Builder
	constrained := 0
	for i := 0; i < len(query); {
		start := nextLogQLSelectorStart(query, i)
		if start < 0 {
			out.WriteString(query[i:])
			break
		}
		end, ok := matchingLogQLSelectorEnd(query, start)
		if !ok {
			return "", fmt.Errorf("parse LogQL query: unmatched { in %q", query)
		}

		candidate := query[start : end+1]
		merged, err := MergeMetricSelectors(candidate, policySelector)
		if err != nil {
			out.WriteString(query[i : end+1])
			i = end + 1
			continue
		}
		out.WriteString(query[i:start])
		out.WriteString(merged)
		constrained++
		i = end + 1
	}

	if constrained == 0 {
		return "", ErrReadPolicyUnsupported
	}
	return out.String(), nil
}

func nextLogQLSelectorStart(s string, from int) int {
	inSingle, inDouble, inBacktick := false, false, false
	escaped := false
	for i := from; i < len(s); i++ {
		ch := s[i]
		switch {
		case escaped:
			escaped = false
		case inBacktick:
			if ch == '`' {
				inBacktick = false
			}
		case inSingle:
			if ch == '\\' {
				escaped = true
			} else if ch == '\'' {
				inSingle = false
			}
		case inDouble:
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inDouble = false
			}
		default:
			switch ch {
			case '`':
				inBacktick = true
			case '\'':
				inSingle = true
			case '"':
				inDouble = true
			case '{':
				return i
			}
		}
	}
	return -1
}

func matchingLogQLSelectorEnd(s string, start int) (int, bool) {
	inSingle, inDouble, inBacktick := false, false, false
	escaped := false
	for i := start + 1; i < len(s); i++ {
		ch := s[i]
		switch {
		case escaped:
			escaped = false
		case inBacktick:
			if ch == '`' {
				inBacktick = false
			}
		case inSingle:
			if ch == '\\' {
				escaped = true
			} else if ch == '\'' {
				inSingle = false
			}
		case inDouble:
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inDouble = false
			}
		default:
			switch ch {
			case '`':
				inBacktick = true
			case '\'':
				inSingle = true
			case '"':
				inDouble = true
			case '}':
				return i, true
			}
		}
	}
	return 0, false
}
