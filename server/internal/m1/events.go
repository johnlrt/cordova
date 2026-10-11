package m1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cordova/server/internal/ranker"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
)

const usageTopic = "cordova.account.usage.v1"

type Events struct {
	redis            *redis.Client
	producer         *kgo.Client
	consumer         *kgo.Client
	topic            string
	consumerName     string
	nodeConsumerName string
}

func NewEvents(client *redis.Client, brokers string) (*Events, error) {
	if client == nil || strings.TrimSpace(brokers) == "" {
		return nil, errors.New("Redis and Kafka brokers are required")
	}
	seeds := strings.Split(brokers, ",")
	producer, err := kgo.NewClient(kgo.SeedBrokers(seeds...), kgo.RequiredAcks(kgo.AllISRAcks()), kgo.RecordRetries(8))
	if err != nil {
		return nil, fmt.Errorf("configure Kafka producer: %w", err)
	}
	consumer, err := kgo.NewClient(kgo.SeedBrokers(seeds...), kgo.ConsumerGroup("cordova-usage-projection-v1"), kgo.ConsumeTopics(usageTopic), kgo.DisableAutoCommit())
	if err != nil {
		producer.Close()
		return nil, fmt.Errorf("configure Kafka consumer: %w", err)
	}
	name, err := randomToken(12)
	if err != nil {
		producer.Close()
		consumer.Close()
		return nil, err
	}
	return &Events{redis: client, producer: producer, consumer: consumer, topic: usageTopic,
		consumerName: "gateway-" + name, nodeConsumerName: "node-publisher-" + name}, nil
}

func (e *Events) Close() { e.producer.Close(); e.consumer.Close() }

func (e *Events) Ping(ctx context.Context) error {
	if err := e.producer.Ping(ctx); err != nil {
		return errors.New("Kafka producer is unavailable")
	}
	if err := e.consumer.Ping(ctx); err != nil {
		return errors.New("Kafka consumer is unavailable")
	}
	return nil
}

func (e *Events) Run(ctx context.Context) error {
	if err := e.ensureOutboxGroup(ctx); err != nil {
		return err
	}
	if err := e.ensureNodeOutboxGroup(ctx); err != nil {
		return err
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errorsCh := make(chan error, 3)
	go func() { errorsCh <- e.runOutbox(workerCtx) }()
	go func() { errorsCh <- e.runProjection(workerCtx) }()
	go func() { errorsCh <- e.runNodeOutbox(workerCtx) }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errorsCh:
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
}

func (e *Events) ensureNodeOutboxGroup(ctx context.Context) error {
	err := e.redis.XGroupCreateMkStream(ctx, nodeEventsOutbox, "node-event-publisher-v1", "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("create node event outbox group: %w", err)
	}
	return nil
}

func (e *Events) runNodeOutbox(ctx context.Context) error {
	for ctx.Err() == nil {
		messages, _, err := e.redis.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream: nodeEventsOutbox, Group: "node-event-publisher-v1", Consumer: e.nodeConsumerName,
			MinIdle: 30 * time.Second, Start: "0-0", Count: 32,
		}).Result()
		if err != nil && err != redis.Nil {
			if ctx.Err() != nil {
				return nil
			}
			if !wait(ctx, time.Second) {
				return nil
			}
			continue
		}
		if len(messages) > 0 {
			if err := e.publishNodeBatch(ctx, messages); err != nil {
				if !wait(ctx, time.Second) {
					return nil
				}
			}
			continue
		}
		streams, err := e.redis.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: "node-event-publisher-v1", Consumer: e.nodeConsumerName,
			Streams: []string{nodeEventsOutbox, ">"}, Count: 32, Block: time.Second,
		}).Result()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !wait(ctx, time.Second) {
				return nil
			}
			continue
		}
		for _, stream := range streams {
			if err := e.publishNodeBatch(ctx, stream.Messages); err != nil {
				if !wait(ctx, time.Second) {
					return nil
				}
				break
			}
		}
	}
	return nil
}

type nodeKafkaEnvelope struct {
	V          int             `json:"v"`
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

func (e *Events) publishNodeBatch(ctx context.Context, messages []redis.XMessage) error {
	for _, message := range messages {
		record, err := nodeEventKafkaRecord(message)
		if err != nil {
			return err
		}
		if err := e.producer.ProduceSync(ctx, record).FirstErr(); err != nil {
			return errors.New("Kafka rejected node metadata")
		}
		if err := appendNodeEventAck(ctx, e.redis, nodeEventsOutbox, "node-event-publisher-v1", message.ID); err != nil {
			return errors.New("node event outbox acknowledgement failed")
		}
	}
	return nil
}

func nodeEventKafkaRecord(message redis.XMessage) (*kgo.Record, error) {
	textField := func(name string) (string, error) {
		value, ok := message.Values[name].(string)
		if !ok {
			return "", fmt.Errorf("node event outbox is missing %s", name)
		}
		return value, nil
	}
	eventID, err := textField("event_id")
	if err != nil {
		return nil, err
	}
	eventType, err := textField("event_type")
	if err != nil {
		return nil, err
	}
	nodeID, err := textField("node_id")
	if err != nil {
		return nil, err
	}
	requestIDText, err := textField("request_id")
	if err != nil {
		return nil, err
	}
	attemptIDText, err := textField("attempt_id")
	if err != nil {
		return nil, err
	}
	sequenceText, err := textField("sequence")
	if err != nil {
		return nil, err
	}
	sequence, err := strconv.ParseUint(sequenceText, 10, 64)
	if err != nil || sequence == 0 {
		return nil, errors.New("node event outbox sequence is invalid")
	}
	occurredText, err := textField("occurred_ms")
	if err != nil {
		return nil, err
	}
	observedText, err := textField("observed_ms")
	if err != nil {
		return nil, err
	}
	occurredMS, err := strconv.ParseInt(occurredText, 10, 64)
	if err != nil || occurredMS < 0 {
		return nil, errors.New("node event occurrence time is invalid")
	}
	observedMS, err := strconv.ParseInt(observedText, 10, 64)
	if err != nil || observedMS < 0 {
		return nil, errors.New("node event observation time is invalid")
	}
	payloadText, err := textField("payload")
	if err != nil || !json.Valid([]byte(payloadText)) {
		return nil, errors.New("node event payload is invalid")
	}
	var requestID, attemptID *string
	if requestIDText != "" {
		requestID = &requestIDText
	}
	if attemptIDText != "" {
		attemptID = &attemptIDText
	}
	observedAt := time.UnixMilli(observedMS).UTC()
	envelope := nodeKafkaEnvelope{
		V: 1, EventID: eventID, EventType: eventType, OccurredAt: time.UnixMilli(occurredMS).UTC(),
		ObservedAt: observedAt, NodeID: nodeID, RequestID: requestID, AttemptID: attemptID,
		Sequence: sequence, Payload: json.RawMessage(payloadText),
	}
	value, err := json.Marshal(envelope)
	if err != nil {
		return nil, errors.New("node event envelope serialization failed")
	}
	if _, err := ranker.DecodeNodeEvent(value); err != nil {
		return nil, errors.New("node event envelope failed C7 validation")
	}
	return &kgo.Record{Topic: ranker.NodeEventsTopic, Key: []byte(nodeID), Value: value, Timestamp: observedAt}, nil
}

func (e *Events) ensureOutboxGroup(ctx context.Context) error {
	err := e.redis.XGroupCreateMkStream(ctx, "cv:v1:outbox:usage", "usage-publisher-v1", "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("create usage outbox group: %w", err)
	}
	return nil
}

func (e *Events) runOutbox(ctx context.Context) error {
	for ctx.Err() == nil {
		messages, _, err := e.redis.XAutoClaim(ctx, &redis.XAutoClaimArgs{Stream: "cv:v1:outbox:usage", Group: "usage-publisher-v1", Consumer: e.consumerName, MinIdle: time.Millisecond, Start: "0-0", Count: 32}).Result()
		if err != nil && err != redis.Nil {
			return fmt.Errorf("read pending usage outbox: %w", err)
		}
		if err == nil && len(messages) > 0 {
			if err := e.publishBatch(ctx, messages); err != nil {
				if !wait(ctx, time.Second) {
					return nil
				}
				continue
			}
			// Drain pending entries in ID order before reading new entries. This
			// keeps per-account Kafka order stable after a partial batch failure.
			continue
		}
		streams, err := e.redis.XReadGroup(ctx, &redis.XReadGroupArgs{Group: "usage-publisher-v1", Consumer: e.consumerName, Streams: []string{"cv:v1:outbox:usage", ">"}, Count: 32, Block: time.Second}).Result()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !wait(ctx, time.Second) {
				return nil
			}
			continue
		}
		for _, stream := range streams {
			if err := e.publishBatch(ctx, stream.Messages); err != nil {
				if !wait(ctx, time.Second) {
					return nil
				}
				break
			}
		}
	}
	return nil
}

func (e *Events) publishBatch(ctx context.Context, messages []redis.XMessage) error {
	for _, message := range messages {
		payload, ok := message.Values["payload"].(string)
		if !ok {
			return errors.New("usage outbox payload is not text")
		}
		accountID, ok := message.Values["account_id"].(string)
		if !ok {
			return errors.New("usage outbox partition key is missing")
		}
		var event usageEvent
		if err := decodeStrictBytes([]byte(payload), &event); err != nil {
			return errors.New("usage outbox event is invalid")
		}
		if event.Payload.AccountID != accountID {
			return errors.New("usage outbox account key mismatch")
		}
		record := &kgo.Record{Topic: e.topic, Key: []byte(accountID), Value: []byte(payload), Timestamp: time.Now().UTC()}
		if err := e.producer.ProduceSync(ctx, record).FirstErr(); err != nil {
			return errors.New("Kafka rejected usage metadata")
		}
		if err := e.redis.Eval(ctx, `
local stream_type=redis.call('TYPE',KEYS[1]).ok
if stream_type~='stream' then return redis.error_reply('usage outbox stream type mismatch') end
redis.call('XACK',KEYS[1],ARGV[1],ARGV[2])
redis.call('XDEL',KEYS[1],ARGV[2])
return 1`, []string{"cv:v1:outbox:usage"}, "usage-publisher-v1", message.ID).Err(); err != nil {
			return errors.New("usage outbox acknowledgement failed")
		}
	}
	return nil
}

func (e *Events) runProjection(ctx context.Context) error {
	for ctx.Err() == nil {
		fetches := e.consumer.PollFetches(ctx)
		if fetches.IsClientClosed() {
			return errors.New("Kafka consumer closed")
		}
		if err := fetches.Err(); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return errors.New("Kafka usage projection fetch failed")
		}
		for _, record := range fetches.Records() {
			var event usageEvent
			if err := decodeStrictBytes(record.Value, &event); err != nil {
				return errors.New("Kafka usage event is invalid")
			}
			if err := e.project(ctx, event); err != nil {
				return errors.New("usage projection failed")
			}
			if err := e.consumer.CommitRecords(ctx, record); err != nil {
				return errors.New("Kafka usage offset commit failed")
			}
		}
	}
	return nil
}

func (e *Events) project(ctx context.Context, event usageEvent) error {
	return projectUsage(ctx, e.redis, event)
}

func decodeStrictBytes(data []byte, target any) error {
	if !json.Valid(data) || duplicateJSON(data) != nil {
		return errors.New("invalid JSON")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(new(any)) == nil {
		return errors.New("multiple JSON values")
	}
	return nil
}

func mustJSON(value any) string { data, _ := json.Marshal(value); return string(data) }
func wait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
