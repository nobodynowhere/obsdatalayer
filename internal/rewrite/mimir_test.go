package rewrite

import (
	"testing"

	gogoproto "github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"

	"obsdatalayer/internal/config"
)

// makeMimirProtoBody builds a snappy+proto Mimir WriteRequest.
func makeMimirProtoBody(ts []timeSeries) []byte {
	req := writeRequest{Timeseries: ts}
	b, err := gogoproto.Marshal(&req)
	if err != nil {
		panic(err)
	}
	return snappy.Encode(nil, b)
}

// decodeMimirProtoBody decodes a snappy+proto Mimir WriteRequest.
func decodeMimirProtoBody(b []byte) (*writeRequest, error) {
	decoded, err := snappy.Decode(nil, b)
	if err != nil {
		return nil, err
	}
	var req writeRequest
	if err := gogoproto.Unmarshal(decoded, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

func TestRewriteMimirNilCfg(t *testing.T) {
	body := makeMimirProtoBody([]timeSeries{
		{Labels: []label{{Name: "app", Value: "foo"}}, Samples: []sample{{Value: 1.0, Timestamp: 1000}}},
	})
	out, err := RewriteMimir(body, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(body) {
		t.Error("expected body unchanged for nil cfg")
	}
}

func TestRewriteMimirAllowlistFilter(t *testing.T) {
	body := makeMimirProtoBody([]timeSeries{
		{
			Labels: []label{
				{Name: "__name__", Value: "http_requests"},
				{Name: "app", Value: "foo"},
				{Name: "env", Value: "staging"},
			},
			Samples: []sample{{Value: 42.0, Timestamp: 1000}},
		},
	})
	cfg := &config.LabelsConfig{
		Filter: &config.FilterConfig{Mode: "allowlist", Names: []string{"__name__", "app"}},
	}
	out, err := RewriteMimir(body, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	req, err := decodeMimirProtoBody(out)
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(req.Timeseries) != 1 {
		t.Fatalf("expected 1 time series, got %d", len(req.Timeseries))
	}
	labelMap := labelsToMap(req.Timeseries[0].Labels)
	if labelMap["__name__"] != "http_requests" {
		t.Errorf("expected __name__='http_requests', got %q", labelMap["__name__"])
	}
	if labelMap["app"] != "foo" {
		t.Errorf("expected app='foo', got %q", labelMap["app"])
	}
	if _, ok := labelMap["env"]; ok {
		t.Error("expected 'env' to be removed by allowlist filter")
	}
}

func TestRewriteMimirDenylistFilter(t *testing.T) {
	body := makeMimirProtoBody([]timeSeries{
		{
			Labels: []label{
				{Name: "app", Value: "foo"},
				{Name: "env", Value: "staging"},
			},
			Samples: []sample{{Value: 1.0, Timestamp: 1000}},
		},
	})
	cfg := &config.LabelsConfig{
		Filter: &config.FilterConfig{Mode: "denylist", Names: []string{"env"}},
	}
	out, err := RewriteMimir(body, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	req, err := decodeMimirProtoBody(out)
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	labelMap := labelsToMap(req.Timeseries[0].Labels)
	if _, ok := labelMap["env"]; ok {
		t.Error("expected 'env' to be removed by denylist filter")
	}
	if labelMap["app"] != "foo" {
		t.Errorf("expected app='foo', got %q", labelMap["app"])
	}
}

func TestRewriteMimirInject(t *testing.T) {
	body := makeMimirProtoBody([]timeSeries{
		{
			Labels:  []label{{Name: "app", Value: "foo"}},
			Samples: []sample{{Value: 1.0, Timestamp: 1000}},
		},
	})
	cfg := &config.LabelsConfig{
		Inject: map[string]string{"cluster": "prod-1"},
	}
	out, err := RewriteMimir(body, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	req, err := decodeMimirProtoBody(out)
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	labelMap := labelsToMap(req.Timeseries[0].Labels)
	if labelMap["cluster"] != "prod-1" {
		t.Errorf("expected cluster='prod-1' injected, got %q", labelMap["cluster"])
	}
	if labelMap["app"] != "foo" {
		t.Errorf("expected app='foo', got %q", labelMap["app"])
	}
}

func TestRewriteMimirMultipleTimeSeries(t *testing.T) {
	body := makeMimirProtoBody([]timeSeries{
		{
			Labels:  []label{{Name: "app", Value: "foo"}, {Name: "env", Value: "staging"}},
			Samples: []sample{{Value: 1.0, Timestamp: 1000}},
		},
		{
			Labels:  []label{{Name: "app", Value: "bar"}, {Name: "env", Value: "staging"}},
			Samples: []sample{{Value: 2.0, Timestamp: 2000}},
		},
	})
	cfg := &config.LabelsConfig{
		Filter: &config.FilterConfig{Mode: "denylist", Names: []string{"env"}},
	}
	out, err := RewriteMimir(body, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	req, err := decodeMimirProtoBody(out)
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(req.Timeseries) != 2 {
		t.Fatalf("expected 2 time series, got %d", len(req.Timeseries))
	}
	for i, ts := range req.Timeseries {
		labelMap := labelsToMap(ts.Labels)
		if _, ok := labelMap["env"]; ok {
			t.Errorf("time series %d: expected 'env' to be removed", i)
		}
	}
}

func TestRewriteMimirLabelsSorted(t *testing.T) {
	body := makeMimirProtoBody([]timeSeries{
		{
			Labels: []label{
				{Name: "z_label", Value: "z"},
				{Name: "a_label", Value: "a"},
				{Name: "m_label", Value: "m"},
			},
			Samples: []sample{{Value: 1.0, Timestamp: 1000}},
		},
	})
	cfg := &config.LabelsConfig{
		Inject: map[string]string{"new_label": "x"},
	}
	out, err := RewriteMimir(body, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	req, err := decodeMimirProtoBody(out)
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	labels := req.Timeseries[0].Labels
	for i := 1; i < len(labels); i++ {
		if labels[i-1].Name >= labels[i].Name {
			t.Errorf("labels not sorted: %q >= %q", labels[i-1].Name, labels[i].Name)
		}
	}
}

func TestRewriteMimirSamplesPreserved(t *testing.T) {
	body := makeMimirProtoBody([]timeSeries{
		{
			Labels: []label{{Name: "app", Value: "foo"}},
			Samples: []sample{
				{Value: 3.14, Timestamp: 12345},
				{Value: 2.71, Timestamp: 67890},
			},
		},
	})
	cfg := &config.LabelsConfig{
		Inject: map[string]string{"cluster": "prod"},
	}
	out, err := RewriteMimir(body, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	req, err := decodeMimirProtoBody(out)
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	samples := req.Timeseries[0].Samples
	if len(samples) != 2 {
		t.Fatalf("expected 2 samples, got %d", len(samples))
	}
	if samples[0].Value != 3.14 || samples[0].Timestamp != 12345 {
		t.Errorf("sample 0 not preserved: %+v", samples[0])
	}
	if samples[1].Value != 2.71 || samples[1].Timestamp != 67890 {
		t.Errorf("sample 1 not preserved: %+v", samples[1])
	}
}

func TestRewriteMimirCorruptSnappy(t *testing.T) {
	body := []byte("not valid snappy data at all!!!!")
	cfg := &config.LabelsConfig{
		Inject: map[string]string{"env": "prod"},
	}
	_, err := RewriteMimir(body, cfg)
	if err == nil {
		t.Fatal("expected error for corrupt snappy body")
	}
}
