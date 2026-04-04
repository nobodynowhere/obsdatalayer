package rewrite

import (
	"testing"

	"obsdatalayer/internal/config"
)

// ---- ParseLabelString tests ----

func TestParseLabelStringEmpty(t *testing.T) {
	m, err := ParseLabelString("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("expected empty map, got %v", m)
	}
}

func TestParseLabelStringEmptyBraces(t *testing.T) {
	m, err := ParseLabelString("{}")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("expected empty map, got %v", m)
	}
}

func TestParseLabelStringSingleLabel(t *testing.T) {
	m, err := ParseLabelString(`{app="foo"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m["app"] != "foo" {
		t.Errorf("expected app='foo', got %q", m["app"])
	}
}

func TestParseLabelStringMultipleLabels(t *testing.T) {
	m, err := ParseLabelString(`{app="foo",env="prod"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m["app"] != "foo" {
		t.Errorf("expected app='foo', got %q", m["app"])
	}
	if m["env"] != "prod" {
		t.Errorf("expected env='prod', got %q", m["env"])
	}
}

func TestParseLabelStringWhitespace(t *testing.T) {
	m, err := ParseLabelString(`{app = "foo" , env = "prod"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m["app"] != "foo" {
		t.Errorf("expected app='foo', got %q", m["app"])
	}
	if m["env"] != "prod" {
		t.Errorf("expected env='prod', got %q", m["env"])
	}
}

func TestParseLabelStringEscapedQuote(t *testing.T) {
	m, err := ParseLabelString(`{msg="say \"hi\""}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m["msg"] != `say "hi"` {
		t.Errorf("expected msg='say \"hi\"', got %q", m["msg"])
	}
}

func TestParseLabelStringEscapedNewline(t *testing.T) {
	m, err := ParseLabelString(`{msg="line1\nline2"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m["msg"] != "line1\nline2" {
		t.Errorf("expected msg with newline, got %q", m["msg"])
	}
}

func TestParseLabelStringEscapedBackslash(t *testing.T) {
	m, err := ParseLabelString(`{path="C:\\Users"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m["path"] != `C:\Users` {
		t.Errorf("expected path='C:\\Users', got %q", m["path"])
	}
}

func TestParseLabelStringNoBraces(t *testing.T) {
	m, err := ParseLabelString(`app="foo"`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m["app"] != "foo" {
		t.Errorf("expected app='foo', got %q", m["app"])
	}
}

func TestParseLabelStringMissingEqualsError(t *testing.T) {
	_, err := ParseLabelString(`{appfoo}`)
	if err == nil {
		t.Fatal("expected error for missing '='")
	}
}

func TestParseLabelStringValueNotQuotedError(t *testing.T) {
	_, err := ParseLabelString(`{app=foo}`)
	if err == nil {
		t.Fatal("expected error for unquoted value")
	}
}

func TestParseLabelStringMultiplePreserved(t *testing.T) {
	m, err := ParseLabelString(`{a="1",b="2",c="3"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m) != 3 {
		t.Errorf("expected 3 labels, got %d", len(m))
	}
	if m["a"] != "1" || m["b"] != "2" || m["c"] != "3" {
		t.Errorf("labels not preserved correctly: %v", m)
	}
}

// ---- FormatLabelString tests ----

func TestFormatLabelStringEmpty(t *testing.T) {
	s := FormatLabelString(map[string]string{})
	if s != "{}" {
		t.Errorf("expected '{}', got %q", s)
	}
}

func TestFormatLabelStringSingle(t *testing.T) {
	s := FormatLabelString(map[string]string{"app": "foo"})
	if s != `{app="foo"}` {
		t.Errorf("expected '{app=\"foo\"}', got %q", s)
	}
}

func TestFormatLabelStringMultipleSorted(t *testing.T) {
	s := FormatLabelString(map[string]string{"env": "prod", "app": "foo"})
	if s != `{app="foo",env="prod"}` {
		t.Errorf("expected '{app=\"foo\",env=\"prod\"}', got %q", s)
	}
}

func TestFormatLabelStringRoundTrip(t *testing.T) {
	orig := map[string]string{"app": "foo", "env": "prod", "team": "platform"}
	formatted := FormatLabelString(orig)
	parsed, err := ParseLabelString(formatted)
	if err != nil {
		t.Fatalf("round-trip parse error: %v", err)
	}
	for k, v := range orig {
		if parsed[k] != v {
			t.Errorf("round-trip: key %q expected %q, got %q", k, v, parsed[k])
		}
	}
}

func TestFormatLabelStringSpecialCharsEscaped(t *testing.T) {
	s := FormatLabelString(map[string]string{"msg": `say "hi"`})
	// Parse back to verify the escaping is correct
	parsed, err := ParseLabelString(s)
	if err != nil {
		t.Fatalf("parse after format error: %v", err)
	}
	if parsed["msg"] != `say "hi"` {
		t.Errorf("expected msg='say \"hi\"', got %q", parsed["msg"])
	}
}

// ---- ApplyFilter tests ----

func TestApplyFilterNil(t *testing.T) {
	input := map[string]string{"app": "foo", "env": "prod"}
	result := ApplyFilter(input, nil)
	if result["app"] != "foo" || result["env"] != "prod" {
		t.Errorf("expected input unchanged, got %v", result)
	}
}

func TestApplyFilterAllowlistKeepsListed(t *testing.T) {
	input := map[string]string{"app": "foo", "env": "prod", "team": "platform"}
	filter := &config.FilterConfig{Mode: "allowlist", Names: []string{"app"}}
	result := ApplyFilter(input, filter)
	if result["app"] != "foo" {
		t.Errorf("expected app='foo' to pass, got %q", result["app"])
	}
	if _, ok := result["env"]; ok {
		t.Error("expected env to be dropped")
	}
	if _, ok := result["team"]; ok {
		t.Error("expected team to be dropped")
	}
}

func TestApplyFilterAllowlistDropsUnlisted(t *testing.T) {
	input := map[string]string{"app": "foo", "env": "prod"}
	filter := &config.FilterConfig{Mode: "allowlist", Names: []string{"app"}}
	result := ApplyFilter(input, filter)
	if _, ok := result["env"]; ok {
		t.Error("expected env to be dropped by allowlist")
	}
}

func TestApplyFilterDenylistDropsListed(t *testing.T) {
	input := map[string]string{"app": "foo", "env": "prod"}
	filter := &config.FilterConfig{Mode: "denylist", Names: []string{"env"}}
	result := ApplyFilter(input, filter)
	if _, ok := result["env"]; ok {
		t.Error("expected env to be dropped by denylist")
	}
	if result["app"] != "foo" {
		t.Errorf("expected app='foo' to pass, got %q", result["app"])
	}
}

func TestApplyFilterDenylistKeepsUnlisted(t *testing.T) {
	input := map[string]string{"app": "foo", "env": "prod"}
	filter := &config.FilterConfig{Mode: "denylist", Names: []string{"env"}}
	result := ApplyFilter(input, filter)
	if result["app"] != "foo" {
		t.Errorf("expected app='foo' to remain, got %q", result["app"])
	}
}

func TestApplyFilterAllowlistEmptyNames(t *testing.T) {
	input := map[string]string{"app": "foo", "env": "prod"}
	filter := &config.FilterConfig{Mode: "allowlist", Names: []string{}}
	result := ApplyFilter(input, filter)
	if len(result) != 0 {
		t.Errorf("expected empty result, got %v", result)
	}
}

func TestApplyFilterDenylistEmptyNames(t *testing.T) {
	input := map[string]string{"app": "foo", "env": "prod"}
	filter := &config.FilterConfig{Mode: "denylist", Names: []string{}}
	result := ApplyFilter(input, filter)
	if len(result) != 2 {
		t.Errorf("expected all labels to pass, got %v", result)
	}
}

func TestApplyFilterUnknownMode(t *testing.T) {
	input := map[string]string{"app": "foo"}
	filter := &config.FilterConfig{Mode: "random", Names: []string{"app"}}
	result := ApplyFilter(input, filter)
	// Unknown mode returns input unchanged
	if result["app"] != "foo" {
		t.Errorf("expected input unchanged for unknown mode, got %v", result)
	}
}

// ---- InjectLabels tests ----

func TestInjectLabelsNilInject(t *testing.T) {
	input := map[string]string{"app": "foo"}
	result := InjectLabels(input, nil)
	if result["app"] != "foo" {
		t.Errorf("expected input unchanged, got %v", result)
	}
}

func TestInjectLabelsEmptyInject(t *testing.T) {
	input := map[string]string{"app": "foo"}
	result := InjectLabels(input, map[string]string{})
	if result["app"] != "foo" {
		t.Errorf("expected input unchanged, got %v", result)
	}
}

func TestInjectLabelsAddsNewKeys(t *testing.T) {
	input := map[string]string{"app": "foo"}
	inject := map[string]string{"env": "prod"}
	result := InjectLabels(input, inject)
	if result["env"] != "prod" {
		t.Errorf("expected env='prod' to be injected, got %q", result["env"])
	}
	if result["app"] != "foo" {
		t.Errorf("expected app='foo' to remain, got %q", result["app"])
	}
}

func TestInjectLabelsOverwritesExistingKeys(t *testing.T) {
	input := map[string]string{"app": "foo", "env": "staging"}
	inject := map[string]string{"env": "prod"}
	result := InjectLabels(input, inject)
	if result["env"] != "prod" {
		t.Errorf("expected env='prod' to overwrite, got %q", result["env"])
	}
}

func TestInjectLabelsDoesNotMutateOriginal(t *testing.T) {
	input := map[string]string{"app": "foo"}
	inject := map[string]string{"env": "prod"}
	_ = InjectLabels(input, inject)
	if _, ok := input["env"]; ok {
		t.Error("expected original map to not be mutated")
	}
}

// ---- ApplyLabelConfig tests ----

func TestApplyLabelConfigNil(t *testing.T) {
	input := map[string]string{"app": "foo"}
	result := ApplyLabelConfig(input, nil)
	if result["app"] != "foo" {
		t.Errorf("expected input unchanged for nil cfg, got %v", result)
	}
}

func TestApplyLabelConfigFilterOnly(t *testing.T) {
	input := map[string]string{"app": "foo", "env": "prod"}
	cfg := &config.LabelsConfig{
		Filter: &config.FilterConfig{Mode: "allowlist", Names: []string{"app"}},
	}
	result := ApplyLabelConfig(input, cfg)
	if result["app"] != "foo" {
		t.Errorf("expected app='foo', got %q", result["app"])
	}
	if _, ok := result["env"]; ok {
		t.Error("expected env to be filtered out")
	}
}

func TestApplyLabelConfigInjectOnly(t *testing.T) {
	input := map[string]string{"app": "foo"}
	cfg := &config.LabelsConfig{
		Inject: map[string]string{"env": "prod"},
	}
	result := ApplyLabelConfig(input, cfg)
	if result["app"] != "foo" {
		t.Errorf("expected app='foo', got %q", result["app"])
	}
	if result["env"] != "prod" {
		t.Errorf("expected env='prod', got %q", result["env"])
	}
}

func TestApplyLabelConfigFilterThenInject(t *testing.T) {
	// Filter removes env, then inject adds cluster
	input := map[string]string{"app": "foo", "env": "staging"}
	cfg := &config.LabelsConfig{
		Filter: &config.FilterConfig{Mode: "allowlist", Names: []string{"app"}},
		Inject: map[string]string{"cluster": "prod-1"},
	}
	result := ApplyLabelConfig(input, cfg)
	if result["app"] != "foo" {
		t.Errorf("expected app='foo', got %q", result["app"])
	}
	if _, ok := result["env"]; ok {
		t.Error("expected env to be filtered out")
	}
	if result["cluster"] != "prod-1" {
		t.Errorf("expected cluster='prod-1' to be injected, got %q", result["cluster"])
	}
}

func TestApplyLabelConfigInjectNotFilteredOut(t *testing.T) {
	// Inject adds a label that is NOT in the allowlist - it should still appear
	input := map[string]string{"app": "foo", "env": "staging"}
	cfg := &config.LabelsConfig{
		Filter: &config.FilterConfig{Mode: "allowlist", Names: []string{"app"}},
		Inject: map[string]string{"injected": "yes"},
	}
	result := ApplyLabelConfig(input, cfg)
	if result["injected"] != "yes" {
		t.Errorf("expected injected='yes' to appear even though not in allowlist, got %q", result["injected"])
	}
}
