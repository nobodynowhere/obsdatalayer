package rewrite

import (
	"encoding/json"
	"testing"

	gogoproto "github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"

	"obsdatalayer/internal/config"
)

// makeLocalLokiProtoBody builds a snappy+proto Loki push body from lokiStream slices.
func makeLocalLokiProtoBody(streams []lokiStream) []byte {
	req := lokiPushRequest{Streams: streams}
	b, err := gogoproto.Marshal(&req)
	if err != nil {
		panic(err)
	}
	return snappy.Encode(nil, b)
}

// decodeLokiProtoBody decodes a snappy+proto Loki push body.
func decodeLokiProtoBody(b []byte) (*lokiPushRequest, error) {
	decoded, err := snappy.Decode(nil, b)
	if err != nil {
		return nil, err
	}
	var req lokiPushRequest
	if err := gogoproto.Unmarshal(decoded, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

const jsonContentType = "application/json"
const protoContentType = "application/x-protobuf"

// ---- RewriteLoki JSON tests ----

func TestRewriteLokiJSONNilCfg(t *testing.T) {
	body := []byte(`{"streams":[{"stream":{"app":"foo"},"values":[["1","log"]]}]}`)
	out, err := RewriteLoki(jsonContentType, body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("expected body unchanged, got %s", out)
	}
}

func TestRewriteLokiJSONUnknownContentType(t *testing.T) {
	body := []byte("raw body")
	out, err := RewriteLoki("text/plain", body, &config.LabelsConfig{
		Inject: map[string]string{"env": "prod"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("expected body unchanged for unknown content type, got %s", out)
	}
}

func TestRewriteLokiJSONDenylistFilter(t *testing.T) {
	body := []byte(`{"streams":[{"stream":{"app":"foo","env":"staging"},"values":[["1","log"]]}]}`)
	cfg := &config.LabelsConfig{
		Filter: &config.FilterConfig{Mode: "denylist", Names: []string{"env"}},
	}
	out, err := RewriteLoki(jsonContentType, body, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var push lokiPushJSON
	if err := json.Unmarshal(out, &push); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if len(push.Streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(push.Streams))
	}
	if _, ok := push.Streams[0].Stream["env"]; ok {
		t.Error("expected 'env' to be removed by denylist filter")
	}
	if push.Streams[0].Stream["app"] != "foo" {
		t.Errorf("expected app='foo', got %q", push.Streams[0].Stream["app"])
	}
}

func TestRewriteLokiJSONAllowlistFilter(t *testing.T) {
	body := []byte(`{"streams":[{"stream":{"app":"foo","env":"staging","team":"platform"},"values":[["1","log"]]}]}`)
	cfg := &config.LabelsConfig{
		Filter: &config.FilterConfig{Mode: "allowlist", Names: []string{"app"}},
	}
	out, err := RewriteLoki(jsonContentType, body, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var push lokiPushJSON
	if err := json.Unmarshal(out, &push); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	stream := push.Streams[0].Stream
	if stream["app"] != "foo" {
		t.Errorf("expected app='foo', got %q", stream["app"])
	}
	if _, ok := stream["env"]; ok {
		t.Error("expected 'env' to be removed by allowlist filter")
	}
	if _, ok := stream["team"]; ok {
		t.Error("expected 'team' to be removed by allowlist filter")
	}
}

func TestRewriteLokiJSONInject(t *testing.T) {
	body := []byte(`{"streams":[{"stream":{"app":"foo"},"values":[["1","log"]]}]}`)
	cfg := &config.LabelsConfig{
		Inject: map[string]string{"cluster": "prod-1"},
	}
	out, err := RewriteLoki(jsonContentType, body, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var push lokiPushJSON
	if err := json.Unmarshal(out, &push); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if push.Streams[0].Stream["cluster"] != "prod-1" {
		t.Errorf("expected cluster='prod-1' injected, got %q", push.Streams[0].Stream["cluster"])
	}
}

func TestRewriteLokiJSONMultipleStreams(t *testing.T) {
	body := []byte(`{"streams":[
		{"stream":{"app":"foo","env":"staging"},"values":[["1","log"]]},
		{"stream":{"app":"bar","env":"staging"},"values":[["2","log2"]]}
	]}`)
	cfg := &config.LabelsConfig{
		Filter: &config.FilterConfig{Mode: "denylist", Names: []string{"env"}},
	}
	out, err := RewriteLoki(jsonContentType, body, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var push lokiPushJSON
	if err := json.Unmarshal(out, &push); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if len(push.Streams) != 2 {
		t.Fatalf("expected 2 streams, got %d", len(push.Streams))
	}
	for i, stream := range push.Streams {
		if _, ok := stream.Stream["env"]; ok {
			t.Errorf("stream %d: expected 'env' to be removed", i)
		}
	}
}

func TestRewriteLokiJSONInjectAfterFilter(t *testing.T) {
	// Injected label 'env' is NOT in allowlist but should still appear
	body := []byte(`{"streams":[{"stream":{"app":"foo","env":"staging"},"values":[["1","log"]]}]}`)
	cfg := &config.LabelsConfig{
		Filter: &config.FilterConfig{Mode: "allowlist", Names: []string{"app"}},
		Inject: map[string]string{"env": "prod"},
	}
	out, err := RewriteLoki(jsonContentType, body, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var push lokiPushJSON
	if err := json.Unmarshal(out, &push); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	// env should be prod (injected), not staging (filtered out then re-injected)
	if push.Streams[0].Stream["env"] != "prod" {
		t.Errorf("expected env='prod' from inject, got %q", push.Streams[0].Stream["env"])
	}
}

func TestRewriteLokiJSONMalformedError(t *testing.T) {
	body := []byte(`{not valid json}`)
	cfg := &config.LabelsConfig{
		Inject: map[string]string{"env": "prod"},
	}
	_, err := RewriteLoki(jsonContentType, body, cfg)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

// ---- RewriteLoki proto tests ----

func TestRewriteLokiProtoNilCfg(t *testing.T) {
	body := makeLocalLokiProtoBody([]lokiStream{
		{Labels: `{app="foo"}`, Entries: []lokiEntry{{Line: "log"}}},
	})
	out, err := RewriteLoki(protoContentType, body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(body) {
		t.Error("expected body unchanged for nil cfg")
	}
}

func TestRewriteLokiProtoDenylistFilter(t *testing.T) {
	body := makeLocalLokiProtoBody([]lokiStream{
		{Labels: `{app="foo",env="staging"}`, Entries: []lokiEntry{{Line: "log"}}},
	})
	cfg := &config.LabelsConfig{
		Filter: &config.FilterConfig{Mode: "denylist", Names: []string{"env"}},
	}
	out, err := RewriteLoki(protoContentType, body, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	req, err := decodeLokiProtoBody(out)
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	labels, err := ParseLabelString(req.Streams[0].Labels)
	if err != nil {
		t.Fatalf("parse labels: %v", err)
	}
	if _, ok := labels["env"]; ok {
		t.Error("expected 'env' to be removed by denylist filter")
	}
	if labels["app"] != "foo" {
		t.Errorf("expected app='foo', got %q", labels["app"])
	}
}

func TestRewriteLokiProtoAllowlistFilter(t *testing.T) {
	body := makeLocalLokiProtoBody([]lokiStream{
		{Labels: `{app="foo",env="staging",team="platform"}`, Entries: []lokiEntry{{Line: "log"}}},
	})
	cfg := &config.LabelsConfig{
		Filter: &config.FilterConfig{Mode: "allowlist", Names: []string{"app"}},
	}
	out, err := RewriteLoki(protoContentType, body, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	req, err := decodeLokiProtoBody(out)
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	labels, err := ParseLabelString(req.Streams[0].Labels)
	if err != nil {
		t.Fatalf("parse labels: %v", err)
	}
	if labels["app"] != "foo" {
		t.Errorf("expected app='foo', got %q", labels["app"])
	}
	if _, ok := labels["env"]; ok {
		t.Error("expected 'env' to be removed")
	}
	if _, ok := labels["team"]; ok {
		t.Error("expected 'team' to be removed")
	}
}

func TestRewriteLokiProtoInject(t *testing.T) {
	body := makeLocalLokiProtoBody([]lokiStream{
		{Labels: `{app="foo"}`, Entries: []lokiEntry{{Line: "log"}}},
	})
	cfg := &config.LabelsConfig{
		Inject: map[string]string{"cluster": "prod-1"},
	}
	out, err := RewriteLoki(protoContentType, body, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	req, err := decodeLokiProtoBody(out)
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	labels, err := ParseLabelString(req.Streams[0].Labels)
	if err != nil {
		t.Fatalf("parse labels: %v", err)
	}
	if labels["cluster"] != "prod-1" {
		t.Errorf("expected cluster='prod-1' injected, got %q", labels["cluster"])
	}
}

func TestRewriteLokiProtoMultipleStreams(t *testing.T) {
	body := makeLocalLokiProtoBody([]lokiStream{
		{Labels: `{app="foo",env="staging"}`, Entries: []lokiEntry{{Line: "log1"}}},
		{Labels: `{app="bar",env="staging"}`, Entries: []lokiEntry{{Line: "log2"}}},
	})
	cfg := &config.LabelsConfig{
		Filter: &config.FilterConfig{Mode: "denylist", Names: []string{"env"}},
	}
	out, err := RewriteLoki(protoContentType, body, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	req, err := decodeLokiProtoBody(out)
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(req.Streams) != 2 {
		t.Fatalf("expected 2 streams, got %d", len(req.Streams))
	}
	for i, stream := range req.Streams {
		labels, err := ParseLabelString(stream.Labels)
		if err != nil {
			t.Fatalf("stream %d: parse labels: %v", i, err)
		}
		if _, ok := labels["env"]; ok {
			t.Errorf("stream %d: expected 'env' to be removed", i)
		}
	}
}

func TestRewriteLokiProtoCorruptSnappy(t *testing.T) {
	body := []byte("not valid snappy data at all!!!!")
	cfg := &config.LabelsConfig{
		Inject: map[string]string{"env": "prod"},
	}
	_, err := RewriteLoki(protoContentType, body, cfg)
	if err == nil {
		t.Fatal("expected error for corrupt snappy body")
	}
}
