package m1

import (
	"context"
	"encoding/json"
	"errors"

	"cordova/server/internal/domain"
	"github.com/redis/go-redis/v9"
)

const (
	nodeEventsOutbox         = "cv:v1:outbox:node"
	maxNodeEventPayloadBytes = 60 << 10
)

func validSafeNodeEventCode(value string) bool {
	switch value {
	case "disconnected", "shutdown", "upgrade":
		return true
	default:
		return false
	}
}

// appendNodeEvent durably records only server-validated, content-free node
// observations. A per-node sequence preserves the topic key's logical order.
func (s *nodeService) appendNodeEvent(ctx context.Context, nodeID, eventType string, requestID, attemptID *string, payload any) error {
	if ctx == nil || s == nil || s.redis == nil || !validID(nodeID) {
		return ErrInvalid
	}
	eventID, payloadJSON, requestValue, attemptValue, err := prepareNodeEvent(nodeID, eventType, requestID, attemptID, payload)
	if err != nil {
		return err
	}
	sequence, err := s.redis.Eval(ctx, appendNodeEventScript, []string{
		nodeEventSequenceKey(nodeID), nodeEventsOutbox,
	}, eventID, eventType, nodeID, requestValue, attemptValue, payloadJSON).Int64()
	if err != nil {
		return errors.New("node event outbox append failed")
	}
	if sequence <= 0 {
		return errors.New("node event outbox sequence is invalid")
	}
	return nil
}

func prepareNodeEvent(nodeID, eventType string, requestID, attemptID *string, payload any) (string, string, string, string, error) {
	if !validID(nodeID) {
		return "", "", "", "", ErrInvalid
	}
	if eventType != "node.join" && eventType != "node.leave" && eventType != "node.heartbeat" &&
		eventType != "node.benchmark" && eventType != "node.request_outcome" {
		return "", "", "", "", ErrInvalid
	}
	if !validOptionalNodeID(requestID) || !validOptionalNodeID(attemptID) {
		return "", "", "", "", ErrInvalid
	}
	eventID, err := domain.NewID()
	if err != nil {
		return "", "", "", "", err
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil || len(payloadJSON) == 0 || len(payloadJSON) > maxNodeEventPayloadBytes {
		return "", "", "", "", ErrInvalid
	}
	requestValue, attemptValue := "", ""
	if requestID != nil {
		requestValue = *requestID
	}
	if attemptID != nil {
		attemptValue = *attemptID
	}
	return eventID.String(), string(payloadJSON), requestValue, attemptValue, nil
}

func nodeEventSequenceKey(nodeID string) string { return "cv:v1:outbox-sequence:node:" + nodeID }

const appendNodeEventScript = `
local sequence_type=redis.call('TYPE',KEYS[1]).ok
local outbox_type=redis.call('TYPE',KEYS[2]).ok
if (sequence_type~='none' and sequence_type~='string') or (outbox_type~='none' and outbox_type~='stream') then
  return redis.error_reply('node event outbox key type mismatch')
end
local sequence=redis.call('INCR',KEYS[1])
local clock=redis.call('TIME')
local observed_ms=tonumber(clock[1])*1000+math.floor(tonumber(clock[2])/1000)
redis.call('XADD',KEYS[2],'*',
  'event_id',ARGV[1], 'event_type',ARGV[2], 'node_id',ARGV[3],
  'request_id',ARGV[4], 'attempt_id',ARGV[5], 'sequence',tostring(sequence),
  'occurred_ms',tostring(observed_ms), 'observed_ms',tostring(observed_ms), 'payload',ARGV[6])
return sequence
`

func appendNodeEventAck(ctx context.Context, client *redis.Client, stream, group, messageID string) error {
	return client.Eval(ctx, `
local stream_type=redis.call('TYPE',KEYS[1]).ok
if stream_type~='stream' then return redis.error_reply('node event outbox stream type mismatch') end
redis.call('XACK',KEYS[1],ARGV[1],ARGV[2])
redis.call('XDEL',KEYS[1],ARGV[2])
return 1`, []string{stream}, group, messageID).Err()
}
