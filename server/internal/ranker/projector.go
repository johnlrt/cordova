package ranker

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/redis/go-redis/v9"
)

const (
	NodeEventsTopic                = "cordova.node.events.v1"
	NodeRankTopic                  = "cordova.node.rank.v1"
	RankerConsumerGroup            = "ranker-v1"
	projectedEventRetentionSeconds = 48 * 60 * 60
	eventDedupRetentionSeconds     = 7 * 24 * 60 * 60
)

type ApplyStatus int64

const (
	EventInserted ApplyStatus = iota + 1
	OffsetAlreadyApplied
	DuplicateEvent
	StaleSequence
	StaleSession
)

var consumerGroupPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

const applyEventScript = `
local function key_type(key)
  return redis.call('TYPE', key).ok
end
local stream_type = key_type(KEYS[1])
local offset_type = key_type(KEYS[2])
local sequence_type = key_type(KEYS[3])
local event_type = key_type(KEYS[4])
local session_type = key_type(KEYS[5])
local nodes_type = key_type(KEYS[6])
if (stream_type ~= 'none' and stream_type ~= 'stream') or
   (offset_type ~= 'none' and offset_type ~= 'string') or
   (sequence_type ~= 'none' and sequence_type ~= 'string') or
   (event_type ~= 'none' and event_type ~= 'string') or
   (session_type ~= 'none' and session_type ~= 'string') or
   (nodes_type ~= 'none' and nodes_type ~= 'set') then
  return redis.error_reply('rank projection key type mismatch')
end
local previous_offset = redis.call('GET', KEYS[2])
if previous_offset and ARGV[1] <= previous_offset then
  return 2
end
local previous_sequence = redis.call('GET', KEYS[3])
if redis.call('EXISTS', KEYS[4]) == 1 then
  redis.call('SET', KEYS[2], ARGV[1])
  return 3
end
if previous_sequence and ARGV[2] <= previous_sequence then
  redis.call('SET', KEYS[2], ARGV[1])
  return 4
end
local current_session = redis.call('GET', KEYS[5])
if (ARGV[15] == 'heartbeat' or ARGV[15] == 'leave') and current_session ~= ARGV[14] then
  redis.call('SET', KEYS[2], ARGV[1])
  return 5
end
local clock = redis.call('TIME')
local now_ms = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
redis.call('XADD', KEYS[1], '*',
  'event_id', ARGV[3],
  'event_type', ARGV[4],
  'occurred_at', ARGV[5],
  'observed_at', ARGV[6],
  'node_id', ARGV[7],
  'request_id', ARGV[8],
  'attempt_id', ARGV[9],
  'sequence', ARGV[2],
  'payload', ARGV[10])
redis.call('XTRIM', KEYS[1], 'MINID', '~', tostring(now_ms - ARGV[11]) .. '-0')
redis.call('EXPIRE', KEYS[1], ARGV[12])
redis.call('SET', KEYS[3], ARGV[2])
redis.call('SET', KEYS[4], '1', 'EX', ARGV[13])
if ARGV[15] == 'join' then
  redis.call('SET', KEYS[5], ARGV[14])
elseif ARGV[15] == 'leave' then
  redis.call('DEL', KEYS[5])
end
redis.call('SET', KEYS[2], ARGV[1])
redis.call('SADD', KEYS[6], ARGV[7])
return 1
`

// Apply atomically stores a validated content-free node event and its Kafka
// offset. Stream IDs use Redis arrival time; node clocks never set window age.
func Apply(ctx context.Context, client *redis.Client, group string, partition int32, offset int64, data []byte) (ApplyStatus, error) {
	if ctx == nil || client == nil || !consumerGroupPattern.MatchString(group) || partition < 0 || offset < 0 {
		return 0, ErrInvalidEvent
	}
	event, err := DecodeNodeEvent(data)
	if err != nil {
		return 0, err
	}
	return applyDecodedEvent(ctx, client, group, partition, offset, event)
}

func applyDecodedEvent(ctx context.Context, client *redis.Client, group string, partition int32, offset int64, event NodeEvent) (ApplyStatus, error) {
	if ctx == nil || client == nil || !consumerGroupPattern.MatchString(group) || partition < 0 || offset < 0 ||
		!validUUID(event.EventID) || !validUUID(event.NodeID) || event.Sequence == 0 {
		return 0, ErrInvalidEvent
	}
	payload, err := jsonMarshal(event.Payload)
	if err != nil {
		return 0, ErrInvalidEvent
	}
	partitionText := fmt.Sprintf("%d", partition)
	keys := []string{
		"cv:v1:rank-events:" + event.NodeID,
		"cv:v1:consumer-offset:" + group + ":" + NodeEventsTopic + ":" + partitionText,
		"cv:v1:rank-sequence:" + event.NodeID,
		"cv:v1:ranker-event:" + event.EventID,
		"cv:v1:ranker-session:" + event.NodeID,
		"cv:v1:ranker-nodes",
	}
	requestID, attemptID := "", ""
	if event.RequestID != nil {
		requestID = *event.RequestID
	}
	if event.AttemptID != nil {
		attemptID = *event.AttemptID
	}
	sessionID, sessionAction := "", ""
	switch payload := event.Payload.(type) {
	case JoinPayload:
		sessionID, sessionAction = payload.SessionID, "join"
	case LeavePayload:
		sessionID, sessionAction = payload.SessionID, "leave"
	case HeartbeatPayload:
		sessionID, sessionAction = payload.SessionID, "heartbeat"
	}
	args := []any{
		fmt.Sprintf("%020d", offset), fmt.Sprintf("%020d", event.Sequence), event.EventID, event.EventType,
		event.OccurredAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		event.ObservedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), event.NodeID, requestID, attemptID, payload,
		projectedEventRetentionSeconds * 1000, projectedEventRetentionSeconds, eventDedupRetentionSeconds,
		sessionID, sessionAction,
	}
	result, err := client.Eval(ctx, applyEventScript, keys, args...).Int64()
	if err != nil {
		return 0, err
	}
	switch ApplyStatus(result) {
	case EventInserted, OffsetAlreadyApplied, DuplicateEvent, StaleSequence, StaleSession:
		return ApplyStatus(result), nil
	default:
		return 0, fmt.Errorf("unexpected rank projection result %d", result)
	}
}

func jsonMarshal(value any) (string, error) {
	data, err := json.Marshal(value)
	return string(data), err
}
