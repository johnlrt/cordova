package ranker

import (
	"errors"
	"fmt"
	"testing"
)

const testNodeID = "00000000-0000-4000-8000-000000000001"

func testEvent(eventType, payload, requestID, attemptID string, sequence int) []byte {
	return testEventForNode(testNodeID, eventType, payload, requestID, attemptID, sequence)
}

func testEventForNode(nodeID, eventType, payload, requestID, attemptID string, sequence int) []byte {
	eventID := fmt.Sprintf("00000000-0000-4000-8000-%012d", sequence)
	return testEventWithID(nodeID, eventID, eventType, payload, requestID, attemptID, uint64(sequence))
}

func testEventWithID(nodeID, eventID, eventType, payload, requestID, attemptID string, sequence uint64) []byte {
	return []byte(fmt.Sprintf(`{"v":1,"event_id":%q,"event_type":%q,"occurred_at":"2026-10-11T10:00:00Z","observed_at":"2026-10-11T10:00:01Z","node_id":%q,"request_id":%s,"attempt_id":%s,"sequence":%d,"payload":%s}`,
		eventID, eventType, nodeID, requestID, attemptID, sequence, payload))
}

func TestDecodeNodeEventAcceptsC7EventTypes(t *testing.T) {
	tests := []struct {
		name        string
		eventType   string
		payload     string
		requestID   string
		attemptID   string
		wantPayload any
	}{
		{"join", "node.join", `{"session_id":"00000000-0000-4000-8000-000000000010"}`, "null", "null", JoinPayload{}},
		{"leave", "node.leave", `{"session_id":"00000000-0000-4000-8000-000000000010","reason":"session_replaced"}`, "null", "null", LeavePayload{}},
		{"heartbeat", "node.heartbeat", `{"session_id":"00000000-0000-4000-8000-000000000010","latency_ms":17,"model_states":[{"model_id":"cordova-chat-small-v1","state":"loaded","active_slots":1,"max_slots":2,"verified_capacity_bytes":4294967296,"updated_at":"2026-10-11T10:00:00Z"}]}`, "null", "null", HeartbeatPayload{}},
		{"benchmark", "node.benchmark", `{"benchmark_id":"00000000-0000-4000-8000-000000000020","model_id":"cordova-chat-small-v1","tested_capacity_bytes":4294967296,"verified_slots":1,"verified_output_tokens":128,"elapsed_ms":5000,"passed":true}`, "null", "null", BenchmarkPayload{}},
		{"outcome", "node.request_outcome", `{"model_id":"cordova-chat-small-v1","outcome":"success","elapsed_ms":1000,"first_token_ms":null,"verified_output_tokens":25,"claim_deviation":false}`, `"00000000-0000-4000-8000-000000000030"`, `"00000000-0000-4000-8000-000000000040"`, RequestOutcomePayload{}},
		{"canary", "node.canary", `{"canary_id":"00000000-0000-4000-8000-000000000050","model_id":"cordova-chat-small-v1","canary_version":"arith-json-v1","passed":false,"failure_code":"property_mismatch"}`, "null", "null", CanaryPayload{Passed: false, FailureCode: "property_mismatch"}},
		{"passing canary", "node.canary", `{"canary_id":"00000000-0000-4000-8000-000000000051","model_id":"cordova-chat-small-v1","canary_version":"arith-json-v1","passed":true,"failure_code":""}`, "null", "null", CanaryPayload{Passed: true}},
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event, err := DecodeNodeEvent(testEvent(test.eventType, test.payload, test.requestID, test.attemptID, i+1))
			if err != nil {
				t.Fatal(err)
			}
			if event.EventType != test.eventType || event.NodeID != testNodeID || event.Sequence != uint64(i+1) {
				t.Fatalf("decoded event envelope = %+v", event)
			}
			switch test.wantPayload.(type) {
			case JoinPayload:
				if _, ok := event.Payload.(JoinPayload); !ok {
					t.Fatalf("payload type %T", event.Payload)
				}
			case LeavePayload:
				if _, ok := event.Payload.(LeavePayload); !ok {
					t.Fatalf("payload type %T", event.Payload)
				}
			case HeartbeatPayload:
				payload, ok := event.Payload.(HeartbeatPayload)
				if !ok || len(payload.ModelStates) != 1 {
					t.Fatalf("payload = %#v", event.Payload)
				}
			case BenchmarkPayload:
				payload, ok := event.Payload.(BenchmarkPayload)
				if !ok || payload.Passed == nil || !*payload.Passed {
					t.Fatalf("payload = %#v", event.Payload)
				}
			case RequestOutcomePayload:
				payload, ok := event.Payload.(RequestOutcomePayload)
				if !ok || payload.FirstTokenMS != nil || event.RequestID == nil || event.AttemptID == nil {
					t.Fatalf("payload = %#v", event.Payload)
				}
			case CanaryPayload:
				payload, ok := event.Payload.(CanaryPayload)
				want := test.wantPayload.(CanaryPayload)
				if !ok || payload.Passed != want.Passed || payload.FailureCode != want.FailureCode || event.RequestID != nil || event.AttemptID != nil {
					t.Fatalf("payload = %#v", event.Payload)
				}
			}
		})
	}
}

func TestDecodeNodeEventRejectsDuplicateUnknownAndUnboundedData(t *testing.T) {
	valid := testEvent("node.join", `{"session_id":"00000000-0000-4000-8000-000000000010"}`, "null", "null", 1)
	tests := map[string][]byte{
		"duplicate envelope field":    []byte(`{"v":1,"v":1}`),
		"unknown envelope field":      []byte(string(valid[:len(valid)-1]) + `,"prompt":"do not retain"}`),
		"duplicate payload field":     testEvent("node.join", `{"session_id":"00000000-0000-4000-8000-000000000010","session_id":"00000000-0000-4000-8000-000000000011"}`, "null", "null", 2),
		"unknown payload field":       testEvent("node.request_outcome", `{"model_id":"cordova-chat-small-v1","outcome":"success","elapsed_ms":1000,"first_token_ms":null,"verified_output_tokens":25,"claim_deviation":false,"prompt":"secret"}`, `"00000000-0000-4000-8000-000000000030"`, `"00000000-0000-4000-8000-000000000040"`, 3),
		"missing benchmark passed":    testEvent("node.benchmark", `{"benchmark_id":"00000000-0000-4000-8000-000000000020","model_id":"cordova-chat-small-v1","tested_capacity_bytes":4294967296,"verified_slots":1,"verified_output_tokens":128,"elapsed_ms":5000}`, "null", "null", 4),
		"unsupported event":           testEvent("node.frame", `{}`, "null", "null", 5),
		"canary prompt leakage":       testEvent("node.canary", `{"canary_id":"00000000-0000-4000-8000-000000000050","model_id":"cordova-chat-small-v1","canary_version":"arith-json-v1","passed":false,"failure_code":"property_mismatch","prompt":"secret"}`, "null", "null", 6),
		"canary failure without code": testEvent("node.canary", `{"canary_id":"00000000-0000-4000-8000-000000000050","model_id":"cordova-chat-small-v1","canary_version":"arith-json-v1","passed":false,"failure_code":""}`, "null", "null", 7),
		"raw frame over limit":        bytesRepeat('x', maxNodeEventBytes+1),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeNodeEvent(data); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("error = %v, want ErrInvalidEvent", err)
			}
		})
	}
}

func TestDecodeNodeEventRejectsStaleOrInvalidClaims(t *testing.T) {
	tests := map[string][]byte{
		"sequence zero":       []byte(fmt.Sprintf(`{"v":1,"event_id":"00000000-0000-4000-8000-000000000001","event_type":"node.join","occurred_at":"2026-10-11T10:00:00Z","observed_at":"2026-10-11T10:00:01Z","node_id":%q,"request_id":null,"attempt_id":null,"sequence":0,"payload":{"session_id":"00000000-0000-4000-8000-000000000010"}}`, testNodeID)),
		"invalid outcome id":  testEvent("node.request_outcome", `{"model_id":"cordova-chat-small-v1","outcome":"success","elapsed_ms":1000,"first_token_ms":null,"verified_output_tokens":25,"claim_deviation":false}`, `"not-a-uuid"`, `"00000000-0000-4000-8000-000000000040"`, 2),
		"unknown model state": testEvent("node.heartbeat", `{"session_id":"00000000-0000-4000-8000-000000000010","latency_ms":17,"model_states":[{"model_id":"bad--slug","state":"online","active_slots":2,"max_slots":1,"verified_capacity_bytes":4294967296,"updated_at":"2026-10-11T10:00:00Z"}]}`, "null", "null", 3),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeNodeEvent(data); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("error = %v, want ErrInvalidEvent", err)
			}
		})
	}
}

func bytesRepeat(char byte, count int) []byte {
	data := make([]byte, count)
	for i := range data {
		data[i] = char
	}
	return data
}
