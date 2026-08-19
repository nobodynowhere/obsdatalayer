package rewrite

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"obsdatalayer/internal/auth"
)

var (
	ErrReadPolicyUnsupported = errors.New("mimir read policy cannot be applied to this endpoint")
	ErrReadPolicyAmbiguous   = errors.New("multiple mimir read policy selectors are not supported")
)

// ValidateGrantReadPolicies checks backend-specific read policy syntax before
// grants are persisted. Authorization structure is still validated by auth.
func ValidateGrantReadPolicies(grants []auth.Grant) error {
	for _, grant := range grants {
		selector := strings.TrimSpace(grant.ReadLabelSelector)
		if selector == "" {
			continue
		}
		if grant.Backend != "mimir" || grant.Action != auth.ActionRead {
			return errors.New("read_label_selector is only supported on mimir read grants")
		}
		if _, _, err := singlePolicySelector([]string{selector}); err != nil {
			return fmt.Errorf("read_label_selector %q is not valid PromQL: %w", selector, err)
		}
	}
	return nil
}

// ApplyMimirReadPolicy constrains Mimir read request parameters with the
// resolved label selector policy carried by the auth middleware.
func ApplyMimirReadPolicy(r *http.Request, endpoint string) error {
	ra := auth.FromContext(r.Context())
	if ra == nil || len(ra.LabelSelectors) == 0 {
		return nil
	}

	values, commit, err := mutableRequestValues(r)
	if err != nil {
		return err
	}
	switch endpoint {
	case "query", "query_range", "query_exemplars":
		query := strings.TrimSpace(values.Get("query"))
		if query == "" {
			return errors.New("query parameter is required for a restricted Mimir read")
		}
		rewritten, err := ConstrainPromQL(query, ra.LabelSelectors)
		if err != nil {
			return err
		}
		values.Set("query", rewritten)
	case "labels", "label_values", "series", "search":
		if err := ConstrainMetricSelectorParams(values, ra.LabelSelectors); err != nil {
			return err
		}
	case "metadata", "read", "cardinality":
		return ErrReadPolicyUnsupported
	default:
		return fmt.Errorf("unknown Mimir read endpoint %q", endpoint)
	}
	commit(values)
	return nil
}

func mutableRequestValues(r *http.Request) (url.Values, func(url.Values), error) {
	if r.Method != http.MethodPost {
		return r.URL.Query(), func(values url.Values) {
			r.URL.RawQuery = values.Encode()
		}, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read form body: %w", err)
	}
	_ = r.Body.Close()

	bodyValues := url.Values{}
	if len(body) > 0 {
		mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaType != "" && mediaType != "application/x-www-form-urlencoded" {
			r.Body = io.NopCloser(bytes.NewReader(body))
			return nil, nil, fmt.Errorf("restricted Mimir POST queries must use application/x-www-form-urlencoded")
		}
		bodyValues, err = url.ParseQuery(string(body))
		if err != nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			return nil, nil, fmt.Errorf("parse form body: %w", err)
		}
	}

	if len(bodyValues) > 0 {
		return bodyValues, func(values url.Values) {
			encoded := values.Encode()
			r.Body = io.NopCloser(strings.NewReader(encoded))
			r.ContentLength = int64(len(encoded))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}, nil
	}

	return r.URL.Query(), func(values url.Values) {
		r.URL.RawQuery = values.Encode()
		r.Body = io.NopCloser(bytes.NewReader(nil))
		r.ContentLength = 0
	}, nil
}

// ConstrainPromQL parses query and appends the policy selector's matchers to
// every vector selector inside it.
func ConstrainPromQL(query string, selectors []string) (string, error) {
	_, policyMatchers, err := singlePolicySelector(selectors)
	if err != nil {
		return "", err
	}
	p := parser.NewParser(parser.Options{})
	expr, err := p.ParseExpr(query)
	if err != nil {
		return "", fmt.Errorf("parse PromQL query: %w", err)
	}
	constrained := 0
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		selector, ok := node.(*parser.VectorSelector)
		if !ok {
			return nil
		}
		selector.LabelMatchers = append(selector.LabelMatchers, cloneMatchers(policyMatchers)...)
		constrained++
		return nil
	})
	if constrained == 0 {
		return "", errors.New("restricted Mimir read query contains no metric selector to constrain")
	}
	return expr.String(), nil
}

// ConstrainMetricSelectorParams constrains Prometheus API match[] parameters.
// If no match[] parameter exists, the policy selector becomes the match[].
func ConstrainMetricSelectorParams(values map[string][]string, selectors []string) error {
	policySelector, _, err := singlePolicySelector(selectors)
	if err != nil {
		return err
	}
	matches := values["match[]"]
	if len(matches) == 0 {
		values["match[]"] = []string{policySelector}
		return nil
	}
	merged := make([]string, 0, len(matches))
	for _, match := range matches {
		match = strings.TrimSpace(match)
		if match == "" {
			return errors.New("match[] must not be empty for a restricted Mimir read")
		}
		next, err := MergeMetricSelectors(match, policySelector)
		if err != nil {
			return err
		}
		merged = append(merged, next)
	}
	values["match[]"] = merged
	return nil
}

// MergeMetricSelectors intersects two PromQL metric selectors.
func MergeMetricSelectors(selector, policySelector string) (string, error) {
	p := parser.NewParser(parser.Options{})
	matchers, err := p.ParseMetricSelector(selector)
	if err != nil {
		return "", fmt.Errorf("parse metric selector %q: %w", selector, err)
	}
	policyMatchers, err := p.ParseMetricSelector(policySelector)
	if err != nil {
		return "", fmt.Errorf("parse policy selector %q: %w", policySelector, err)
	}
	return formatMetricSelector(append(matchers, policyMatchers...)), nil
}

func singlePolicySelector(selectors []string) (string, []*labels.Matcher, error) {
	var clean []string
	seen := make(map[string]struct{})
	for _, selector := range selectors {
		selector = strings.TrimSpace(selector)
		if selector == "" {
			continue
		}
		if _, ok := seen[selector]; ok {
			continue
		}
		seen[selector] = struct{}{}
		clean = append(clean, selector)
	}
	if len(clean) == 0 {
		return "", nil, nil
	}
	if len(clean) > 1 {
		return "", nil, ErrReadPolicyAmbiguous
	}
	p := parser.NewParser(parser.Options{})
	matchers, err := p.ParseMetricSelector(clean[0])
	if err != nil {
		return "", nil, fmt.Errorf("parse policy selector %q: %w", clean[0], err)
	}
	return formatMetricSelector(matchers), matchers, nil
}

func cloneMatchers(in []*labels.Matcher) []*labels.Matcher {
	out := make([]*labels.Matcher, len(in))
	copy(out, in)
	return out
}

func formatMetricSelector(matchers []*labels.Matcher) string {
	parts := make([]string, 0, len(matchers))
	for _, matcher := range matchers {
		parts = append(parts, matcher.String())
	}
	sort.Strings(parts)
	return "{" + strings.Join(parts, ",") + "}"
}
