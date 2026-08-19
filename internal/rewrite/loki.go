package rewrite

import (
	"encoding/json"
	"fmt"
	"strings"

	gogoproto "github.com/gogo/protobuf/proto"
	gogoproto_types "github.com/gogo/protobuf/types"
	"github.com/golang/snappy"

	"obsdatalayer/internal/config"
)

// lokiPushJSON represents the Loki push JSON payload.
type lokiPushJSON struct {
	Streams []lokiStreamJSON `json:"streams"`
}

// lokiStreamJSON represents a single log stream in the Loki JSON push payload.
type lokiStreamJSON struct {
	Stream map[string]string `json:"stream"`
	Values [][]string        `json:"values"`
}

// lokiPushRequest is the protobuf type for Loki push.
type lokiPushRequest struct {
	Streams []lokiStream `protobuf:"bytes,1,rep,name=streams,proto3"`
}

func (m *lokiPushRequest) Reset()         { *m = lokiPushRequest{} }
func (m *lokiPushRequest) String() string { return fmt.Sprintf("%+v", *m) }
func (m *lokiPushRequest) ProtoMessage()  {}

// lokiStream is a single stream in the Loki protobuf push payload.
type lokiStream struct {
	Labels  string      `protobuf:"bytes,1,opt,name=labels,proto3"`
	Entries []lokiEntry `protobuf:"bytes,2,rep,name=entries,proto3"`
}

func (m *lokiStream) Reset()         { *m = lokiStream{} }
func (m *lokiStream) String() string { return fmt.Sprintf("%+v", *m) }
func (m *lokiStream) ProtoMessage()  {}

// lokiEntry is a single log entry in the Loki protobuf push payload.
type lokiEntry struct {
	Timestamp *gogoproto_types.Timestamp `protobuf:"bytes,1,opt,name=timestamp,proto3"`
	Line      string                     `protobuf:"bytes,2,opt,name=line,proto3"`
}

func (m *lokiEntry) Reset()         { *m = lokiEntry{} }
func (m *lokiEntry) String() string { return fmt.Sprintf("%+v", *m) }
func (m *lokiEntry) ProtoMessage()  {}

// RewriteLoki rewrites labels in a Loki push payload.
// contentType is the Content-Type header value.
// Supports application/json and application/x-protobuf.
func RewriteLoki(contentType string, body []byte, cfg *config.LabelsConfig) ([]byte, error) {
	out, _, err := RewriteLokiWithStats(contentType, body, cfg)
	return out, err
}

// RewriteLokiWithStats rewrites labels and returns per-payload accounting.
func RewriteLokiWithStats(contentType string, body []byte, cfg *config.LabelsConfig) ([]byte, PayloadStats, error) {
	if cfg == nil {
		stats, err := InspectLoki(contentType, body)
		if err != nil {
			return body, PayloadStats{}, nil
		}
		return body, stats, nil
	}

	if strings.Contains(contentType, "application/json") {
		return rewriteLokiJSON(body, cfg)
	}
	if strings.Contains(contentType, "application/x-protobuf") {
		return rewriteLokiProto(body, cfg)
	}

	// Unknown content type - return as-is
	return body, PayloadStats{}, nil
}

func InspectLoki(contentType string, body []byte) (PayloadStats, error) {
	if strings.Contains(contentType, "application/json") {
		var push lokiPushJSON
		if err := json.Unmarshal(body, &push); err != nil {
			return PayloadStats{}, fmt.Errorf("unmarshal loki JSON: %w", err)
		}
		return PayloadStats{ItemKind: "streams", ItemsTotal: len(push.Streams)}, nil
	}
	if strings.Contains(contentType, "application/x-protobuf") {
		decoded, err := snappy.Decode(nil, body)
		if err != nil {
			return PayloadStats{}, fmt.Errorf("snappy decode: %w", err)
		}
		var req lokiPushRequest
		if err := gogoproto.Unmarshal(decoded, &req); err != nil {
			return PayloadStats{}, fmt.Errorf("proto unmarshal loki: %w", err)
		}
		return PayloadStats{ItemKind: "streams", ItemsTotal: len(req.Streams)}, nil
	}
	return PayloadStats{}, nil
}

func rewriteLokiJSON(body []byte, cfg *config.LabelsConfig) ([]byte, PayloadStats, error) {
	var push lokiPushJSON
	if err := json.Unmarshal(body, &push); err != nil {
		return nil, PayloadStats{}, fmt.Errorf("unmarshal loki JSON: %w", err)
	}

	stats := PayloadStats{ItemKind: "streams"}
	for i := range push.Streams {
		before := push.Streams[i].Stream
		after := ApplyLabelConfig(before, cfg)
		stats.AddItem(before, after)
		push.Streams[i].Stream = after
	}

	out, err := json.Marshal(&push)
	if err != nil {
		return nil, PayloadStats{}, fmt.Errorf("marshal loki JSON: %w", err)
	}
	return out, stats, nil
}

func rewriteLokiProto(body []byte, cfg *config.LabelsConfig) ([]byte, PayloadStats, error) {
	// Snappy decode
	decoded, err := snappy.Decode(nil, body)
	if err != nil {
		return nil, PayloadStats{}, fmt.Errorf("snappy decode: %w", err)
	}

	// Proto unmarshal
	var req lokiPushRequest
	if err := gogoproto.Unmarshal(decoded, &req); err != nil {
		return nil, PayloadStats{}, fmt.Errorf("proto unmarshal loki: %w", err)
	}

	// Rewrite labels
	stats := PayloadStats{ItemKind: "streams"}
	for i := range req.Streams {
		labels, err := ParseLabelString(req.Streams[i].Labels)
		if err != nil {
			return nil, PayloadStats{}, fmt.Errorf("parse loki labels %q: %w", req.Streams[i].Labels, err)
		}
		rewritten := ApplyLabelConfig(labels, cfg)
		stats.AddItem(labels, rewritten)
		req.Streams[i].Labels = FormatLabelString(rewritten)
	}

	// Proto marshal
	marshaled, err := gogoproto.Marshal(&req)
	if err != nil {
		return nil, PayloadStats{}, fmt.Errorf("proto marshal loki: %w", err)
	}

	// Snappy encode
	return snappy.Encode(nil, marshaled), stats, nil
}
