package rewrite

import (
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
