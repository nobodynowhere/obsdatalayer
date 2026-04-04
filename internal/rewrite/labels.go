package rewrite

import (
	"fmt"
	"sort"
	"strings"

	"obsdatalayer/internal/config"
)

// ParseLabelString parses a Loki label string like {a="b",c="d"} into a map.
// Handles backslash escaping in values.
func ParseLabelString(s string) (map[string]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return map[string]string{}, nil
	}

	// Strip surrounding braces
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		s = s[1 : len(s)-1]
	}

	result := make(map[string]string)
	s = strings.TrimSpace(s)
	if s == "" {
		return result, nil
	}

	for len(s) > 0 {
		s = strings.TrimSpace(s)
		if s == "" {
			break
		}

		// Find key
		eqIdx := strings.Index(s, "=")
		if eqIdx < 0 {
			return nil, fmt.Errorf("invalid label string: missing '=' in %q", s)
		}
		key := strings.TrimSpace(s[:eqIdx])
		s = s[eqIdx+1:]

		// Value must start with "
		s = strings.TrimSpace(s)
		if len(s) == 0 || s[0] != '"' {
			return nil, fmt.Errorf("invalid label string: value must start with '\"' for key %q", key)
		}
		s = s[1:] // consume opening quote

		// Parse value with escape handling
		var val strings.Builder
		escaped := false
		i := 0
		for i < len(s) {
			ch := s[i]
			if escaped {
				switch ch {
				case 'n':
					val.WriteByte('\n')
				case 't':
					val.WriteByte('\t')
				case '"':
					val.WriteByte('"')
				case '\\':
					val.WriteByte('\\')
				default:
					val.WriteByte('\\')
					val.WriteByte(ch)
				}
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				i++
				break
			} else {
				val.WriteByte(ch)
			}
			i++
		}
		s = s[i:]

		result[key] = val.String()

		// Skip comma separator
		s = strings.TrimSpace(s)
		if strings.HasPrefix(s, ",") {
			s = s[1:]
		}
	}

	return result, nil
}

// FormatLabelString formats a map as a sorted Loki label string {a="b",c="d"}.
// Keys are sorted alphabetically. Values are escaped.
func FormatLabelString(labels map[string]string) string {
	if len(labels) == 0 {
		return "{}"
	}

	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf(`%s="%s"`, k, escapeLabelValue(labels[k]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}

// ApplyFilter applies allowlist/denylist filter. Returns new filtered map.
func ApplyFilter(labels map[string]string, filter *config.FilterConfig) map[string]string {
	if filter == nil {
		return labels
	}

	nameSet := make(map[string]bool, len(filter.Names))
	for _, n := range filter.Names {
		nameSet[n] = true
	}

	result := make(map[string]string)
	switch filter.Mode {
	case "allowlist":
		for k, v := range labels {
			if nameSet[k] {
				result[k] = v
			}
		}
	case "denylist":
		for k, v := range labels {
			if !nameSet[k] {
				result[k] = v
			}
		}
	default:
		// Unknown mode, return as-is
		return labels
	}
	return result
}

// InjectLabels merges inject map into labels. Inject happens after filter (injected labels are not filtered).
// Returns new map.
func InjectLabels(labels map[string]string, inject map[string]string) map[string]string {
	if len(inject) == 0 {
		return labels
	}
	result := make(map[string]string, len(labels)+len(inject))
	for k, v := range labels {
		result[k] = v
	}
	for k, v := range inject {
		result[k] = v
	}
	return result
}

// ApplyLabelConfig applies filter then inject to a label map.
// If labels config is nil, returns the input unchanged.
func ApplyLabelConfig(labels map[string]string, cfg *config.LabelsConfig) map[string]string {
	if cfg == nil {
		return labels
	}
	if cfg.Filter != nil {
		labels = ApplyFilter(labels, cfg.Filter)
	}
	if len(cfg.Inject) > 0 {
		labels = InjectLabels(labels, cfg.Inject)
	}
	return labels
}
