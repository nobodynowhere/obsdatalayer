package rewrite

import (
	"fmt"
	"sort"

	gogoproto "github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"

	"obsdatalayer/internal/config"
)

// writeRequest is the Prometheus remote write protobuf type.
type writeRequest struct {
	Timeseries []timeSeries `protobuf:"bytes,1,rep,name=timeseries,proto3"`
}

func (m *writeRequest) Reset()         { *m = writeRequest{} }
func (m *writeRequest) String() string { return fmt.Sprintf("%+v", *m) }
func (m *writeRequest) ProtoMessage()  {}

// timeSeries represents a single time series in the remote write payload.
type timeSeries struct {
	Labels  []label  `protobuf:"bytes,1,rep,name=labels,proto3"`
	Samples []sample `protobuf:"bytes,2,rep,name=samples,proto3"`
}

func (m *timeSeries) Reset()         { *m = timeSeries{} }
func (m *timeSeries) String() string { return fmt.Sprintf("%+v", *m) }
func (m *timeSeries) ProtoMessage()  {}

// label is a key-value pair label.
type label struct {
	Name  string `protobuf:"bytes,1,opt,name=name,proto3"`
	Value string `protobuf:"bytes,2,opt,name=value,proto3"`
}

func (m *label) Reset()         { *m = label{} }
func (m *label) String() string { return fmt.Sprintf("%+v", *m) }
func (m *label) ProtoMessage()  {}

// sample is a metric sample.
type sample struct {
	Value     float64 `protobuf:"fixed64,1,opt,name=value,proto3"`
	Timestamp int64   `protobuf:"varint,2,opt,name=timestamp,proto3"`
}

func (m *sample) Reset()         { *m = sample{} }
func (m *sample) String() string { return fmt.Sprintf("%+v", *m) }
func (m *sample) ProtoMessage()  {}

// labelsToMap converts a []label slice to a map[string]string.
func labelsToMap(ls []label) map[string]string {
	m := make(map[string]string, len(ls))
	for _, l := range ls {
		m[l.Name] = l.Value
	}
	return m
}

// mapToLabels converts a map[string]string to a sorted []label slice.
func mapToLabels(m map[string]string) []label {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	ls := make([]label, len(keys))
	for i, k := range keys {
		ls[i] = label{Name: k, Value: m[k]}
	}
	return ls
}

// RewriteMimir rewrites labels in a Mimir remote write payload (snappy+proto WriteRequest).
func RewriteMimir(body []byte, cfg *config.LabelsConfig) ([]byte, error) {
	if cfg == nil {
		return body, nil
	}

	// Snappy decode
	decoded, err := snappy.Decode(nil, body)
	if err != nil {
		return nil, fmt.Errorf("snappy decode: %w", err)
	}

	// Proto unmarshal
	var req writeRequest
	if err := gogoproto.Unmarshal(decoded, &req); err != nil {
		return nil, fmt.Errorf("proto unmarshal mimir: %w", err)
	}

	// Rewrite labels for each time series
	for i := range req.Timeseries {
		labelMap := labelsToMap(req.Timeseries[i].Labels)
		labelMap = ApplyLabelConfig(labelMap, cfg)
		req.Timeseries[i].Labels = mapToLabels(labelMap)
	}

	// Proto marshal
	marshaled, err := gogoproto.Marshal(&req)
	if err != nil {
		return nil, fmt.Errorf("proto marshal mimir: %w", err)
	}

	// Snappy encode
	return snappy.Encode(nil, marshaled), nil
}
