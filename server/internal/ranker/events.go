package ranker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"cordova/server/internal/domain"
)

const maxNodeEventBytes = 64 << 10

var ErrInvalidEvent = errors.New("invalid node event")

type NodeEvent struct {
	EventID    string
	EventType  string
	OccurredAt time.Time
	ObservedAt time.Time
	NodeID     string
	RequestID  *string
	AttemptID  *string
	Sequence   uint64
	Payload    any
}

type ModelState struct {
	ModelID               string    `json:"model_id"`
	State                 string    `json:"state"`
	ActiveSlots           uint32    `json:"active_slots"`
	MaxSlots              uint32    `json:"max_slots"`
	VerifiedCapacityBytes uint64    `json:"verified_capacity_bytes"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type JoinPayload struct {
	SessionID string `json:"session_id"`
}

type LeavePayload struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason"`
}

type HeartbeatPayload struct {
	SessionID   string       `json:"session_id"`
	LatencyMS   uint64       `json:"latency_ms"`
	ModelStates []ModelState `json:"model_states"`
}

type BenchmarkPayload struct {
	BenchmarkID          string `json:"benchmark_id"`
	ModelID              string `json:"model_id"`
	TestedCapacityBytes  uint64 `json:"tested_capacity_bytes"`
	VerifiedSlots        uint32 `json:"verified_slots"`
	VerifiedOutputTokens uint32 `json:"verified_output_tokens"`
	ElapsedMS            uint32 `json:"elapsed_ms"`
	Passed               *bool  `json:"passed"`
}

type RequestOutcomePayload struct {
	ModelID              string  `json:"model_id"`
	Outcome              string  `json:"outcome"`
	ElapsedMS            uint32  `json:"elapsed_ms"`
	FirstTokenMS         *uint32 `json:"first_token_ms"`
	VerifiedOutputTokens uint32  `json:"verified_output_tokens"`
	ClaimDeviation       bool    `json:"claim_deviation"`
}

type CanaryPayload struct {
	CanaryID      string `json:"canary_id"`
	ModelID       string `json:"model_id"`
	CanaryVersion string `json:"canary_version"`
	Passed        bool   `json:"passed"`
	FailureCode   string `json:"failure_code"`
}

type eventEnvelope struct {
	Version    int             `json:"v"`
	EventID    string          `json:"event_id"`
	EventType  string          `json:"event_type"`
	OccurredAt time.Time       `json:"occurred_at"`
	ObservedAt time.Time       `json:"observed_at"`
	NodeID     string          `json:"node_id"`
	RequestID  *string         `json:"request_id"`
	AttemptID  *string         `json:"attempt_id"`
	Sequence   uint64          `json:"sequence"`
	Payload    json.RawMessage `json:"payload"`
}

func DecodeNodeEvent(data []byte) (NodeEvent, error) {
	var envelope eventEnvelope
	if len(data) == 0 || len(data) > maxNodeEventBytes || decodeStrictJSON(data, &envelope) != nil {
		return NodeEvent{}, ErrInvalidEvent
	}
	if requiredFields(data, "v", "event_id", "event_type", "occurred_at", "observed_at", "node_id", "request_id", "attempt_id", "sequence", "payload") != nil ||
		envelope.Version != 1 || !validUUID(envelope.EventID) || !validUUID(envelope.NodeID) || envelope.Sequence == 0 ||
		envelope.OccurredAt.IsZero() || envelope.ObservedAt.IsZero() || len(envelope.Payload) == 0 ||
		!validOptionalUUID(envelope.RequestID) || !validOptionalUUID(envelope.AttemptID) {
		return NodeEvent{}, ErrInvalidEvent
	}

	event := NodeEvent{EventID: envelope.EventID, EventType: envelope.EventType,
		OccurredAt: envelope.OccurredAt, ObservedAt: envelope.ObservedAt, NodeID: envelope.NodeID,
		RequestID: envelope.RequestID, AttemptID: envelope.AttemptID, Sequence: envelope.Sequence}
	switch envelope.EventType {
	case "node.join":
		var payload JoinPayload
		if envelope.RequestID != nil || envelope.AttemptID != nil || decodePayload(envelope.Payload, &payload, "session_id") != nil || !validUUID(payload.SessionID) {
			return NodeEvent{}, ErrInvalidEvent
		}
		event.Payload = payload
	case "node.leave":
		var payload LeavePayload
		if envelope.RequestID != nil || envelope.AttemptID != nil || decodePayload(envelope.Payload, &payload, "session_id", "reason") != nil ||
			!validUUID(payload.SessionID) || !validSafeCode(payload.Reason, 64) {
			return NodeEvent{}, ErrInvalidEvent
		}
		event.Payload = payload
	case "node.heartbeat":
		var raw struct {
			SessionID   string            `json:"session_id"`
			LatencyMS   uint64            `json:"latency_ms"`
			ModelStates []json.RawMessage `json:"model_states"`
		}
		if envelope.RequestID != nil || envelope.AttemptID != nil ||
			decodePayload(envelope.Payload, &raw, "session_id", "latency_ms", "model_states") != nil ||
			!validUUID(raw.SessionID) || raw.LatencyMS > 60_000 || raw.ModelStates == nil || len(raw.ModelStates) > 256 {
			return NodeEvent{}, ErrInvalidEvent
		}
		payload := HeartbeatPayload{SessionID: raw.SessionID, LatencyMS: raw.LatencyMS,
			ModelStates: make([]ModelState, 0, len(raw.ModelStates))}
		seenModels := make(map[string]struct{}, len(raw.ModelStates))
		for _, modelData := range raw.ModelStates {
			var model ModelState
			if decodePayload(modelData, &model, "model_id", "state", "active_slots", "max_slots", "verified_capacity_bytes", "updated_at") != nil ||
				!validModelSlug(model.ModelID) || !validModelState(model.State) || model.ActiveSlots > model.MaxSlots ||
				model.VerifiedCapacityBytes > maxSafeInteger || model.UpdatedAt.IsZero() {
				return NodeEvent{}, ErrInvalidEvent
			}
			if _, duplicate := seenModels[model.ModelID]; duplicate {
				return NodeEvent{}, ErrInvalidEvent
			}
			seenModels[model.ModelID] = struct{}{}
			payload.ModelStates = append(payload.ModelStates, model)
		}
		event.Payload = payload
	case "node.benchmark":
		var payload BenchmarkPayload
		if envelope.RequestID != nil || envelope.AttemptID != nil ||
			decodePayload(envelope.Payload, &payload, "benchmark_id", "model_id", "tested_capacity_bytes", "verified_slots", "verified_output_tokens", "elapsed_ms", "passed") != nil ||
			!validUUID(payload.BenchmarkID) || !validModelSlug(payload.ModelID) || payload.TestedCapacityBytes == 0 ||
			payload.TestedCapacityBytes > maxSafeInteger || payload.VerifiedSlots == 0 || payload.VerifiedSlots > 64 ||
			payload.VerifiedOutputTokens > 1_000_000 || payload.ElapsedMS == 0 || payload.ElapsedMS > 600_000 || payload.Passed == nil {
			return NodeEvent{}, ErrInvalidEvent
		}
		event.Payload = payload
	case "node.request_outcome":
		var payload RequestOutcomePayload
		if !validOptionalUUID(envelope.RequestID) || envelope.RequestID == nil || !validOptionalUUID(envelope.AttemptID) || envelope.AttemptID == nil ||
			decodePayload(envelope.Payload, &payload, "model_id", "outcome", "elapsed_ms", "first_token_ms", "verified_output_tokens", "claim_deviation") != nil ||
			!validModelSlug(payload.ModelID) || (payload.Outcome != "success" && payload.Outcome != "failure" && payload.Outcome != "cancelled") ||
			payload.ElapsedMS == 0 || payload.ElapsedMS > 120_000 ||
			(payload.FirstTokenMS != nil && *payload.FirstTokenMS > payload.ElapsedMS) || payload.VerifiedOutputTokens > 1_000_000 {
			return NodeEvent{}, ErrInvalidEvent
		}
		event.Payload = payload
	case "node.canary":
		var payload CanaryPayload
		if envelope.RequestID != nil || envelope.AttemptID != nil ||
			decodePayload(envelope.Payload, &payload, "canary_id", "model_id", "canary_version", "passed", "failure_code") != nil ||
			!validUUID(payload.CanaryID) || !validModelSlug(payload.ModelID) || !validModelSlug(payload.CanaryVersion) ||
			(payload.Passed && payload.FailureCode != "") || (!payload.Passed && !validCanaryFailureCode(payload.FailureCode)) {
			return NodeEvent{}, ErrInvalidEvent
		}
		event.Payload = payload
	default:
		return NodeEvent{}, ErrInvalidEvent
	}
	return event, nil
}

const maxSafeInteger uint64 = (1 << 53) - 1

func decodePayload(data []byte, target any, fields ...string) error {
	if requiredFields(data, fields...) != nil {
		return ErrInvalidEvent
	}
	return decodeStrictJSON(data, target)
}

func decodeStrictJSON(data []byte, target any) error {
	if rejectDuplicateKeys(data) != nil {
		return ErrInvalidEvent
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("multiple JSON values")
	}
	return nil
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("multiple JSON values")
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not text")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("duplicate object key")
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("invalid object end")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("invalid array end")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func requiredFields(data []byte, fields ...string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return ErrInvalidEvent
	}
	for _, field := range fields {
		if _, ok := object[field]; !ok {
			return fmt.Errorf("%w: missing field %s", ErrInvalidEvent, field)
		}
	}
	return nil
}

func validUUID(value string) bool {
	parsed, err := domain.ParseID(value)
	return err == nil && parsed.String() == value
}

func validOptionalUUID(value *string) bool { return value == nil || validUUID(*value) }

func validModelSlug(value string) bool {
	if value == "" || len(value) > 96 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	previousDash := false
	for _, char := range value {
		if char == '-' {
			if previousDash {
				return false
			}
			previousDash = true
			continue
		}
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return false
		}
		previousDash = false
	}
	return true
}

func validModelState(value string) bool {
	switch value {
	case "loaded", "on_disk", "absent", "downloading", "loading", "failed":
		return true
	default:
		return false
	}
}

func validSafeCode(value string, maxLength int) bool {
	if value == "" || len(value) > maxLength {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && !strings.ContainsRune("._-", char) {
			return false
		}
	}
	return true
}
