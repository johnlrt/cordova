package ranker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
)

func TestInvalidEventDLQContainsOnlyStableMetadata(t *testing.T) {
	input := &kgo.Record{
		Topic: NodeEventsTopic, Partition: 2, Offset: 47, Timestamp: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC),
		Value: []byte(`{"event_id":"00000000-0000-4000-8000-000000000047","prompt":"secret sentinel"}`),
	}
	first, err := buildInvalidEventRecord(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildInvalidEventRecord(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Topic != deadLetterTopic || string(first.Key) != string(second.Key) || string(first.Value) != string(second.Value) {
		t.Fatalf("DLQ output is not stable: first=%+v second=%+v", first, second)
	}
	if !validUUID(string(first.Key)) || strings.Contains(string(first.Value), "secret sentinel") || strings.Contains(string(first.Value), "prompt") {
		t.Fatalf("DLQ output contains invalid ID or raw content: %s", first.Value)
	}
	var decoded invalidEventEnvelope
	if err := json.Unmarshal(first.Value, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.EventID != string(first.Key) || decoded.EventType != "event.invalid" || decoded.Payload.SourceTopic != NodeEventsTopic ||
		decoded.Payload.Partition != 2 || decoded.Payload.Offset != 47 || decoded.Payload.ErrorCode != "invalid_node_event" ||
		decoded.Payload.SourceEventID == nil || *decoded.Payload.SourceEventID != "00000000-0000-4000-8000-000000000047" ||
		decoded.NodeID != nil || decoded.RequestID != nil || decoded.AttemptID != nil {
		t.Fatalf("unexpected metadata-only DLQ event: %+v", decoded)
	}
}

func TestInvalidEventDLQUsesNullSourceIDWhenMalformed(t *testing.T) {
	record := &kgo.Record{Topic: NodeEventsTopic, Partition: 0, Offset: 1, Timestamp: time.Now().UTC(), Value: []byte("not-json")}
	result, err := buildInvalidEventRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	var decoded invalidEventEnvelope
	if err := json.Unmarshal(result.Value, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Payload.SourceEventID != nil {
		t.Fatalf("source event ID = %v, want null", decoded.Payload.SourceEventID)
	}
}
