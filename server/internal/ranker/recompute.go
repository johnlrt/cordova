package ranker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"cordova/server/internal/domain"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
)

const (
	rankWindowDuration      = 24 * time.Hour
	rankHeartbeatInterval   = 15 * time.Second
	rankExpectedIntervals   = uint64(rankWindowDuration / rankHeartbeatInterval)
	rankStreamPageSize      = int64(500)
	rankOutboxStream        = "cv:v1:outbox:rank"
	rankOutboxConsumerGroup = "rank-publisher-v1"
	rankOutboxMinIdle       = 30 * time.Second
)

type rankAggregation struct {
	window          Window
	benchmarkSeen   bool
	benchmarkPassed bool
	benchmarkAt     time.Time
	latencies       []uint64
	heartbeatSlots  map[uint64]struct{}
	benchmarks      map[string]benchmarkObservation
	fraudSignals    []fraudSignal
}

type benchmarkObservation struct {
	payload BenchmarkPayload
	arrival time.Time
}

func newRankAggregation(now time.Time) rankAggregation {
	return rankAggregation{
		window:         Window{Now: now, ExpectedHeartbeatIntervals: rankExpectedIntervals},
		latencies:      make([]uint64, 0),
		heartbeatSlots: make(map[uint64]struct{}),
		benchmarks:     make(map[string]benchmarkObservation),
		fraudSignals:   make([]fraudSignal, 0),
	}
}

func (a *rankAggregation) add(message redis.XMessage) error {
	arrival, err := streamArrival(message.ID)
	if err != nil {
		return err
	}
	eventType, ok := streamField(message.Values, "event_type")
	if !ok {
		return errors.New("rank event has no type")
	}
	payloadText, ok := streamField(message.Values, "payload")
	if !ok {
		return errors.New("rank event has no payload")
	}
	switch eventType {
	case "node.heartbeat":
		var payload HeartbeatPayload
		if err := json.Unmarshal([]byte(payloadText), &payload); err != nil || !validUUID(payload.SessionID) {
			return errors.New("rank heartbeat payload is invalid")
		}
		a.heartbeatSlots[uint64(arrival.UnixMilli())/uint64(rankHeartbeatInterval/time.Millisecond)] = struct{}{}
	case "node.benchmark":
		var payload BenchmarkPayload
		if err := json.Unmarshal([]byte(payloadText), &payload); err != nil || payload.ElapsedMS == 0 || payload.Passed == nil {
			return errors.New("rank benchmark payload is invalid")
		}
		// Stream iteration is in Redis arrival order, so the final benchmark
		// encountered is the latest observed result, including a failed run.
		a.benchmarkSeen = true
		a.benchmarkPassed = *payload.Passed
		a.benchmarkAt = arrival
		a.window.TestedCapacityBytes = payload.TestedCapacityBytes
		a.window.VerifiedOutputTokens = uint64(payload.VerifiedOutputTokens)
		a.window.BenchmarkElapsedMS = uint64(payload.ElapsedMS)
		a.benchmarks[payload.ModelID] = benchmarkObservation{payload: payload, arrival: arrival}
	case "node.request_outcome":
		var payload RequestOutcomePayload
		if err := json.Unmarshal([]byte(payloadText), &payload); err != nil {
			return errors.New("rank outcome payload is invalid")
		}
		a.window.FinishedAttempts++
		if payload.Outcome == "success" {
			a.window.SuccessfulAttempts++
		}
		if payload.FirstTokenMS != nil {
			a.latencies = append(a.latencies, uint64(*payload.FirstTokenMS))
		}
		if payload.ClaimDeviation {
			if err := a.addFraudSignal(message, arrival, fraudClaimDeviation); err != nil {
				return err
			}
		}
		if benchmark, exists := a.benchmarks[payload.ModelID]; exists &&
			!arrival.Before(benchmark.arrival) && arrival.Sub(benchmark.arrival) <= benchmarkMaxAge {
			if code := throughputFraudSignal(payload, benchmark.payload); code != "" {
				if err := a.addFraudSignal(message, arrival, code); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (a *rankAggregation) addFraudSignal(message redis.XMessage, observedAt time.Time, code string) error {
	eventID, ok := streamField(message.Values, "event_id")
	if !ok || !validUUID(eventID) {
		return errors.New("fraud signal source event ID is invalid")
	}
	requestID, ok := streamField(message.Values, "request_id")
	if !ok || !validUUID(requestID) {
		return errors.New("fraud signal request ID is invalid")
	}
	attemptID, ok := streamField(message.Values, "attempt_id")
	if !ok || !validUUID(attemptID) {
		return errors.New("fraud signal attempt ID is invalid")
	}
	a.fraudSignals = append(a.fraudSignals, fraudSignal{SourceEventID: eventID, RequestID: requestID,
		AttemptID: attemptID, Code: code, ObservedAt: observedAt.UTC().Format(time.RFC3339Nano)})
	return nil
}

func (a *rankAggregation) finish() Window {
	a.window.ConnectedHeartbeatIntervals = uint64(len(a.heartbeatSlots))
	a.window.LatencySamples = uint64(len(a.latencies))
	if len(a.latencies) > 0 {
		sort.Slice(a.latencies, func(i, j int) bool { return a.latencies[i] < a.latencies[j] })
		// Nearest-rank p50, i.e. the lower middle value for an even sample count.
		a.window.P50ServerLatencyMillis = a.latencies[(len(a.latencies)-1)/2]
	}
	a.window.BenchmarkPassed = a.benchmarkSeen && a.benchmarkPassed
	a.window.BenchmarkAt = a.benchmarkAt
	return a.window
}

func streamArrival(id string) (time.Time, error) {
	parts := strings.SplitN(id, "-", 2)
	if len(parts) != 2 {
		return time.Time{}, errors.New("invalid Redis stream ID")
	}
	millis, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || millis < 0 {
		return time.Time{}, errors.New("invalid Redis stream time")
	}
	return time.UnixMilli(millis).UTC(), nil
}

func streamField(values map[string]any, key string) (string, bool) {
	value, exists := values[key]
	if !exists {
		return "", false
	}
	switch typed := value.(type) {
	case string:
		return typed, true
	case []byte:
		return string(typed), true
	default:
		return fmt.Sprint(typed), true
	}
}

func (w *Worker) recomputeAll(ctx context.Context) error {
	now, err := w.redis.Time(ctx).Result()
	if err != nil {
		return errors.New("ranker could not read Redis time")
	}
	var cursor uint64
	for {
		nodes, next, err := w.redis.SScan(ctx, "cv:v1:ranker-nodes", cursor, "*", 200).Result()
		if err != nil {
			return errors.New("ranker could not scan node index")
		}
		for _, nodeID := range nodes {
			if !validUUID(nodeID) {
				continue
			}
			if err := w.recomputeNode(ctx, nodeID, now); err != nil {
				return err
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return nil
}

func (w *Worker) recomputeNode(ctx context.Context, nodeID string, now time.Time) error {
	recordData, err := w.redis.Get(ctx, "cv:v1:node:"+nodeID).Bytes()
	if err == redis.Nil {
		_ = w.redis.SRem(ctx, "cv:v1:ranker-nodes", nodeID).Err()
		return nil
	}
	if err != nil {
		return errors.New("ranker could not read node record")
	}
	var record struct {
		ID      string `json:"id"`
		Revoked bool   `json:"revoked"`
	}
	if json.Unmarshal(recordData, &record) != nil || record.ID != nodeID {
		return errors.New("ranker found an invalid node record")
	}
	if record.Revoked {
		if err := w.redis.SRem(ctx, "cv:v1:ranker-nodes", nodeID).Err(); err != nil {
			return errors.New("ranker could not remove revoked node from index")
		}
		if err := w.redis.Del(ctx, "cv:v1:rank:"+nodeID).Err(); err != nil {
			return errors.New("ranker could not remove revoked node rank")
		}
		return nil
	}
	cutoff := now.Add(-rankWindowDuration)
	minimumID := strconv.FormatInt(cutoff.UnixMilli(), 10) + "-0"
	aggregate := newRankAggregation(now)
	streamKey := "cv:v1:rank-events:" + nodeID
	for minimumID != "+" {
		messages, err := w.redis.XRangeN(ctx, streamKey, minimumID, "+", rankStreamPageSize).Result()
		if err != nil && err != redis.Nil {
			return errors.New("ranker could not read node event window")
		}
		if len(messages) == 0 {
			break
		}
		for _, message := range messages {
			if err := aggregate.add(message); err != nil {
				return errors.New("ranker event projection is malformed")
			}
		}
		minimumID = "(" + messages[len(messages)-1].ID
	}
	window := aggregate.finish()
	if err := w.persistFraudSignals(ctx, nodeID, aggregate.fraudSignals); err != nil {
		return errors.New("ranker could not persist fraud review signals")
	}
	quarantined, err := w.redis.Exists(ctx, "cv:v1:node-quarantine:"+nodeID).Result()
	if err != nil {
		return errors.New("ranker could not read node quarantine state")
	}
	window.Quarantined = quarantined > 0
	result, err := Compute(window)
	if err != nil {
		return fmt.Errorf("ranker rejected aggregate window: %w", err)
	}
	updatedAt := now.UTC().Format(time.RFC3339Nano)
	eventID, err := domain.NewID()
	if err != nil {
		return errors.New("ranker could not allocate rank event ID")
	}
	measuredTPS := ""
	testedCapacity := ""
	if aggregate.benchmarkSeen {
		measuredTPS = strconv.FormatFloat(float64(window.VerifiedOutputTokens)*1000/float64(window.BenchmarkElapsedMS), 'f', -1, 64)
		testedCapacity = strconv.FormatUint(window.TestedCapacityBytes, 10)
	}
	latency := ""
	if window.LatencySamples > 0 {
		latency = strconv.FormatUint(window.P50ServerLatencyMillis, 10)
	}
	uptimeRatio := ""
	if window.ExpectedHeartbeatIntervals > 0 {
		uptimeRatio = strconv.FormatFloat(float64(min(window.ConnectedHeartbeatIntervals, window.ExpectedHeartbeatIntervals))/float64(window.ExpectedHeartbeatIntervals), 'f', -1, 64)
	}
	successRatio := ""
	if window.FinishedAttempts > 0 {
		successRatio = strconv.FormatFloat(float64(window.SuccessfulAttempts)/float64(window.FinishedAttempts), 'f', -1, 64)
	}
	_, err = w.redis.Eval(ctx, persistRankScript, []string{
		"cv:v1:rank:" + nodeID, rankOutboxStream,
	}, result.Rank, FormulaVersion, result.Provisional, updatedAt, eventID.String(), nodeID,
		testedCapacity, measuredTPS, latency, uptimeRatio, successRatio).Int64()
	if err != nil {
		return errors.New("ranker could not persist rank and outbox")
	}
	return nil
}

func (w *Worker) persistFraudSignals(ctx context.Context, nodeID string, signals []fraudSignal) error {
	if len(signals) == 0 {
		return nil
	}
	stream := "cv:v1:rank-fraud-signals:" + nodeID
	for _, signal := range signals {
		if !validUUID(signal.SourceEventID) || !validUUID(signal.RequestID) || !validUUID(signal.AttemptID) ||
			(signal.Code != fraudClaimDeviation && signal.Code != fraudHighThroughput && signal.Code != fraudLowThroughput) {
			return errors.New("invalid fraud review signal")
		}
		dedupe := "cv:v1:ranker-fraud-signal:" + nodeID + ":" + signal.SourceEventID + ":" + signal.Code
		if err := w.redis.Eval(ctx, persistFraudSignalScript, []string{stream, dedupe}, signal.SourceEventID,
			signal.RequestID, signal.AttemptID, signal.Code, signal.ObservedAt).Err(); err != nil {
			return err
		}
	}
	return nil
}

const persistFraudSignalScript = `
local stream_type=redis.call('TYPE',KEYS[1]).ok
local dedupe_type=redis.call('TYPE',KEYS[2]).ok
if (stream_type~='none' and stream_type~='stream') or (dedupe_type~='none' and dedupe_type~='string') then
  return redis.error_reply('fraud signal key type mismatch')
end
local inserted=redis.call('SET',KEYS[2],'1','NX','EX',604800)
if inserted then
  local appended=redis.pcall('XADD',KEYS[1],'*','source_event_id',ARGV[1],'request_id',ARGV[2],
    'attempt_id',ARGV[3],'signal_code',ARGV[4],'observed_at',ARGV[5])
  if type(appended)=='table' and appended['err'] then
    redis.call('DEL',KEYS[2])
    return redis.error_reply(appended['err'])
  end
  redis.call('XTRIM',KEYS[1],'MAXLEN','~',10000)
  redis.call('EXPIRE',KEYS[1],604800)
  return 1
end
return 0
`

const persistRankScript = `
local rank_type=redis.call('TYPE',KEYS[1]).ok
local outbox_type=redis.call('TYPE',KEYS[2]).ok
if (rank_type~='none' and rank_type~='hash') or (outbox_type~='none' and outbox_type~='stream') then
  return redis.error_reply('rank output key type mismatch')
end
local old_rank=redis.call('HGET',KEYS[1],'rank')
local old_formula=redis.call('HGET',KEYS[1],'formula_version')
local changed=(old_rank~=ARGV[1] or old_formula~=ARGV[2])
local revision=redis.call('HINCRBY',KEYS[1],'revision',1)
redis.call('HSET',KEYS[1],
  'rank',ARGV[1], 'formula_version',ARGV[2], 'provisional',ARGV[3], 'updated_at',ARGV[4])
for index=7,11 do
  local fields={'tested_capacity_bytes','measured_tokens_per_second','latency_ms','uptime_ratio','success_ratio'}
  if ARGV[index]=='' then redis.call('HDEL',KEYS[1],fields[index-6])
  else redis.call('HSET',KEYS[1],fields[index-6],ARGV[index]) end
end
if changed then
  redis.call('XADD',KEYS[2],'*',
    'event_id',ARGV[5], 'node_id',ARGV[6], 'rank',ARGV[1], 'formula_version',ARGV[2],
    'updated_at',ARGV[4], 'sequence',tostring(revision))
end
return changed and 1 or 0
`

func (w *Worker) ensureRankOutboxGroup(ctx context.Context) error {
	err := w.redis.XGroupCreateMkStream(ctx, rankOutboxStream, rankOutboxConsumerGroup, "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return errors.New("ranker could not initialize rank outbox consumer group")
	}
	return nil
}

type rankChangedEnvelope struct {
	Version    int                `json:"v"`
	EventID    string             `json:"event_id"`
	EventType  string             `json:"event_type"`
	OccurredAt string             `json:"occurred_at"`
	ObservedAt string             `json:"observed_at"`
	NodeID     string             `json:"node_id"`
	RequestID  *string            `json:"request_id"`
	AttemptID  *string            `json:"attempt_id"`
	Sequence   uint64             `json:"sequence"`
	Payload    rankChangedPayload `json:"payload"`
}

type rankChangedPayload struct {
	Rank           int    `json:"rank"`
	FormulaVersion string `json:"formula_version"`
	UpdatedAt      string `json:"updated_at"`
}

func (w *Worker) publishPendingRankChanges(ctx context.Context) error {
	consumer := w.consumerID
	start := "0-0"
	for {
		claimed, next, err := w.redis.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream: rankOutboxStream, Group: rankOutboxConsumerGroup, MinIdle: rankOutboxMinIdle,
			Start: start, Count: 100, Consumer: consumer,
		}).Result()
		if err != nil && err != redis.Nil {
			return errors.New("ranker could not reclaim pending rank events")
		}
		if err := w.publishAndAcknowledge(ctx, consumer, claimed); err != nil {
			return err
		}
		if len(claimed) == 0 || next == "0-0" {
			break
		}
		start = next
	}
	for {
		streams, err := w.redis.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: rankOutboxConsumerGroup, Consumer: consumer,
			Streams: []string{rankOutboxStream, ">"}, Count: 100,
		}).Result()
		if err != nil && err != redis.Nil {
			return errors.New("ranker could not read rank outbox")
		}
		if len(streams) == 0 || len(streams[0].Messages) == 0 {
			break
		}
		for _, stream := range streams {
			if err := w.publishAndAcknowledge(ctx, consumer, stream.Messages); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *Worker) publishAndAcknowledge(ctx context.Context, consumer string, messages []redis.XMessage) error {
	for _, message := range messages {
		event, err := rankOutboxKafkaRecord(message)
		if err != nil {
			return errors.New("rank outbox record is malformed")
		}
		if err := w.producer.ProduceSync(ctx, event).FirstErr(); err != nil {
			return errors.New("rank event Kafka publish failed")
		}
		if err := w.redis.Eval(ctx, acknowledgeRankOutboxScript, []string{rankOutboxStream}, rankOutboxConsumerGroup, message.ID).Err(); err != nil {
			return errors.New("rank event outbox acknowledgement failed")
		}
	}
	return nil
}

const acknowledgeRankOutboxScript = `
redis.call('XACK',KEYS[1],ARGV[1],ARGV[2])
redis.call('XDEL',KEYS[1],ARGV[2])
return 1
`

func rankOutboxKafkaRecord(message redis.XMessage) (*kgo.Record, error) {
	get := func(field string) (string, error) {
		value, ok := streamField(message.Values, field)
		if !ok {
			return "", fmt.Errorf("missing %s", field)
		}
		return value, nil
	}
	eventID, err := get("event_id")
	if err != nil || !validUUID(eventID) {
		return nil, errors.New("invalid outbox event ID")
	}
	nodeID, err := get("node_id")
	if err != nil || !validUUID(nodeID) {
		return nil, errors.New("invalid outbox node ID")
	}
	rankText, err := get("rank")
	if err != nil {
		return nil, err
	}
	rank, err := strconv.Atoi(rankText)
	if err != nil || rank < 0 || rank > 100 {
		return nil, errors.New("invalid outbox rank")
	}
	formula, err := get("formula_version")
	if err != nil || formula != FormulaVersion {
		return nil, errors.New("invalid outbox formula")
	}
	updatedAt, err := get("updated_at")
	if err != nil {
		return nil, err
	}
	if _, err := time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return nil, errors.New("invalid outbox timestamp")
	}
	sequenceText, err := get("sequence")
	if err != nil {
		return nil, err
	}
	sequence, err := strconv.ParseUint(sequenceText, 10, 64)
	if err != nil || sequence == 0 || sequence > math.MaxInt64 {
		return nil, errors.New("invalid outbox sequence")
	}
	envelope := rankChangedEnvelope{
		Version: 1, EventID: eventID, EventType: "node.rank_changed", OccurredAt: updatedAt,
		ObservedAt: updatedAt, NodeID: nodeID, Sequence: sequence,
		Payload: rankChangedPayload{Rank: rank, FormulaVersion: formula, UpdatedAt: updatedAt},
	}
	value, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	timestamp, _ := time.Parse(time.RFC3339Nano, updatedAt)
	return &kgo.Record{Topic: NodeRankTopic, Key: []byte(nodeID), Value: value, Timestamp: timestamp}, nil
}
