package m1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
)

const usageTopic = "cordova.account.usage.v1"

type Events struct {
	redis        *redis.Client
	producer     *kgo.Client
	consumer     *kgo.Client
	topic        string
	consumerName string
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
	return &Events{redis: client, producer: producer, consumer: consumer, topic: usageTopic, consumerName: "gateway-" + name}, nil
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
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- e.runOutbox(workerCtx) }()
	go func() { errorsCh <- e.runProjection(workerCtx) }()
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
