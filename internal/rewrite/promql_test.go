package rewrite

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"obsdatalayer/internal/auth"
)

func TestValidateGrantReadPolicies(t *testing.T) {
	grants := []auth.Grant{{
		Backend:           "mimir",
		Action:            "read",
		TenantIDs:         []string{"tenant-a"},
		ReadLabelSelector: `{cluster="prod"}`,
	}}
	if err := ValidateGrantReadPolicies(grants); err != nil {
		t.Fatalf("expected valid policy, got %v", err)
	}
}

func TestValidateGrantReadPoliciesRejectsInvalidPromQL(t *testing.T) {
	grants := []auth.Grant{{
		Backend:           "mimir",
		Action:            "read",
		TenantIDs:         []string{"tenant-a"},
		ReadLabelSelector: `{cluster=`,
	}}
	if err := ValidateGrantReadPolicies(grants); err == nil {
		t.Fatal("expected invalid PromQL to be rejected")
	}
}

func TestConstrainPromQLAddsPolicyToEveryVectorSelector(t *testing.T) {
	got, err := ConstrainPromQL(`sum(rate(http_requests_total{job="api"}[5m])) / up`, []string{`{cluster="prod"}`})
	if err != nil {
		t.Fatalf("constrain query: %v", err)
	}
	for _, want := range []string{`http_requests_total{cluster="prod",job="api"}`, `up{cluster="prod"}`} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected rewritten query to contain %q, got %q", want, got)
		}
	}
}

func TestConstrainPromQLRejectsScalarOnlyQuery(t *testing.T) {
	if _, err := ConstrainPromQL(`1 + 1`, []string{`{cluster="prod"}`}); err == nil {
		t.Fatal("expected scalar-only query to be rejected")
	}
}

func TestConstrainMetricSelectorParamsAddsMatchWhenMissing(t *testing.T) {
	values := url.Values{}
	if err := ConstrainMetricSelectorParams(values, []string{`{cluster="prod"}`}); err != nil {
		t.Fatalf("constrain params: %v", err)
	}
	if got := values.Get("match[]"); got != `{cluster="prod"}` {
		t.Fatalf("expected policy match[], got %q", got)
	}
}

func TestConstrainMetricSelectorParamsMergesExistingMatches(t *testing.T) {
	values := url.Values{"match[]": []string{`up{job="api"}`, `{namespace="payments"}`}}
	if err := ConstrainMetricSelectorParams(values, []string{`{cluster="prod"}`}); err != nil {
		t.Fatalf("constrain params: %v", err)
	}
	got := values["match[]"]
	want := []string{`{__name__="up",cluster="prod",job="api"}`, `{cluster="prod",namespace="payments"}`}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
}

func TestConstrainPromQLRejectsMultiplePolicySelectors(t *testing.T) {
	if _, err := ConstrainPromQL(`up`, []string{`{cluster="prod"}`, `{team="payments"}`}); err != ErrReadPolicyAmbiguous {
		t.Fatalf("expected ErrReadPolicyAmbiguous, got %v", err)
	}
}

func TestApplyMimirReadPolicyRewritesPostForm(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/mimir/prometheus/api/v1/query", strings.NewReader("query=up"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(auth.WithRequestAuth(req.Context(), &auth.RequestAuth{
		Username:       "alice",
		TenantIDs:      []string{"tenant-a"},
		LabelSelectors: []string{`{cluster="prod"}`},
		IsRead:         true,
	}))

	if err := ApplyMimirReadPolicy(req, "query"); err != nil {
		t.Fatalf("apply policy: %v", err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if got := values.Get("query"); got != `up{cluster="prod"}` {
		t.Fatalf("expected rewritten query, got %q", got)
	}
}

func TestApplyMimirReadPolicyConstrainsSearch(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/prometheus/api/v1/search/metric_names?match[]=up{job=\"api\"}", nil)
	req = req.WithContext(auth.WithRequestAuth(req.Context(), &auth.RequestAuth{
		Username:       "alice",
		TenantIDs:      []string{"tenant-a"},
		LabelSelectors: []string{`{cluster="prod"}`},
		IsRead:         true,
	}))

	if err := ApplyMimirReadPolicy(req, "search"); err != nil {
		t.Fatalf("apply policy: %v", err)
	}
	if got := req.URL.Query().Get("match[]"); got != `{__name__="up",cluster="prod",job="api"}` {
		t.Fatalf("expected constrained search matcher, got %q", got)
	}
}
