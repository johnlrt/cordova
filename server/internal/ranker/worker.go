package ranker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
)

const deadLetterTopic = "cordova.events.dlq.v1"

type Worker struct {
	redis      *redis.Client
	consumer   *kgo.Client
	producer   *kgo.Client
	consumerID string
}

type invalidEventEnvelope struct {
	Version    int                 `json:"v"`
	EventID    string              `json:"event_id"`
	EventType  string              `json:"event_type"`
	OccurredAt time.Time           `json:"occurred_at"`
	ObservedAt time.Time           `json:"observed_at"`
	NodeID     *string             `json:"node_id"`
	RequestID  *string             `json:"request_id"`
	AttemptID  *string             `json:"attempt_id"`
	Sequence   uint64              `json:"sequence"`
	Payload    invalidEventPayload `json:"payload"`
}

type invalidEventPayload struct {
	SourceTopic   string  `json:"source_topic"`
	Partition     int32   `json:"partition"`
	Offset        int64   `json:"offset"`
	ErrorCode     string  `json:"error_code"`
	SourceEventID *string `json:"source_event_id"`
}

func NewWorker(client *redis.Client, brokers string) (*Worker, error) {
	if client == nil || strings.TrimSpace(brokers) == "" {
		return nil, errors.New("Redis and Kafka brokers are required")
	}
	seeds := strings.Split(brokers, ",")
	for index := range seeds {
		seeds[index] = strings.TrimSpace(seeds[index])
		if seeds[index] == "" {
			return nil, errors.New("Kafka broker list is invalid")
		}
	}
	producer, err := kgo.NewClient(kgo.SeedBrokers(seeds...), kgo.RequiredAcks(kgo.AllISRAcks()), kgo.RecordRetries(8))
	if err != nil {
		return nil, fmt.Errorf("configure rank DLQ producer: %w", err)
	}
	consumer, err := kgo.NewClient(kgo.SeedBrokers(seeds...), kgo.ConsumerGroup(RankerConsumerGroup),
		kgo.ConsumeTopics(NodeEventsTopic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()), kgo.DisableAutoCommit())
	if err != nil {
		producer.Close()
		return nil, fmt.Errorf("configure node events consumer: %w", err)
	}
	host, _ := os.Hostname()
	return &Worker{redis: client, producer: producer, consumer: consumer,
		consumerID: fmt.Sprintf("ranker-%s-%d", host, os.Getpid())}, nil
}

func (w *Worker) Close() {
	if w == nil {
		return
	}
	if w.consumer != nil {
		w.consumer.Close()
	}
	if w.producer != nil {
		w.producer.Close()
	}
}

func (w *Worker) Ping(ctx context.Context) error {
	if w == nil || w.redis == nil || w.consumer == nil || w.producer == nil || ctx == nil {
		return errors.New("ranker worker is unavailable")
	}
	if err := w.redis.Ping(ctx).Err(); err != nil {
		return errors.New("ranker Redis is unavailable")
	}
	if err := w.consumer.Ping(ctx); err != nil {
		return errors.New("ranker Kafka consumer is unavailable")
	}
	if err := w.producer.Ping(ctx); err != nil {
		return errors.New("ranker Kafka producer is unavailable")
	}
	return nil
}

func (w *Worker) Run(ctx context.Context) error {
	if w == nil || w.redis == nil || w.consumer == nil || w.producer == nil || ctx == nil {
		return errors.New("ranker worker is unavailable")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	rankerErrors := make(chan error, 1)
	go func() {
		if err := w.runRankLoop(runCtx); err != nil {
			rankerErrors <- err
			cancel()
		}
	}()
	for runCtx.Err() == nil {
		fetches := w.consumer.PollFetches(runCtx)
		if runCtx.Err() != nil {
			break
		}
		if fetches.IsClientClosed() {
			return errors.New("ranker Kafka consumer closed")
		}
		if err := fetches.Err(); err != nil {
			return errors.New("ranker Kafka fetch failed")
		}
		for _, record := range fetches.Records() {
			if err := w.applyRecord(runCtx, record); err != nil {
				cancel()
				return err
			}
		}
	}
	select {
	case err := <-rankerErrors:
		return err
	default:
		return nil
	}
}

func (w *Worker) runRankLoop(ctx context.Context) error {
	if err := w.ensureRankOutboxGroup(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := w.recomputeAll(ctx); err != nil {
			return err
		}
		if err := w.publishPendingRankChanges(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (w *Worker) applyRecord(ctx context.Context, record *kgo.Record) error {
	if record == nil || record.Topic != NodeEventsTopic || record.Offset < 0 {
		return errors.New("ranker received an invalid Kafka record")
	}
	event, err := DecodeNodeEvent(record.Value)
	if err != nil || string(record.Key) != event.NodeID {
		if err := w.publishInvalidEvent(ctx, record); err != nil {
			return err
		}
	} else if _, err := applyDecodedEvent(ctx, w.redis, RankerConsumerGroup, record.Partition, record.Offset, event); err != nil {
		return errors.New("ranker Redis projection failed")
	}
	if err := w.consumer.CommitRecords(ctx, record); err != nil {
		return errors.New("ranker Kafka offset commit failed")
	}
	return nil
}

func (w *Worker) publishInvalidEvent(ctx context.Context, record *kgo.Record) error {
	recordOut, err := buildInvalidEventRecord(record)
	if err != nil {
		return err
	}
	if err := w.producer.ProduceSync(ctx, recordOut).FirstErr(); err != nil {
		return errors.New("ranker DLQ publish failed")
	}
	return nil
}

func buildInvalidEventRecord(record *kgo.Record) (*kgo.Record, error) {
	if record == nil || record.Topic != NodeEventsTopic || record.Offset < 0 {
		return nil, errors.New("invalid source record")
	}
	if record.Timestamp.IsZero() {
		return nil, errors.New("invalid node event lacks a Kafka timestamp")
	}
	eventID := deterministicInvalidEventID(record.Topic, record.Partition, record.Offset)
	var sourceEventID *string
	var partial struct {
		EventID string `json:"event_id"`
	}
	if len(record.Value) <= maxNodeEventBytes && json.Unmarshal(record.Value, &partial) == nil && validUUID(partial.EventID) {
		sourceEventID = &partial.EventID
	}
	timestamp := record.Timestamp.UTC()
	envelope := invalidEventEnvelope{
		Version: 1, EventID: eventID, EventType: "event.invalid", OccurredAt: timestamp,
		ObservedAt: timestamp, Sequence: uint64(record.Offset) + 1,
		Payload: invalidEventPayload{SourceTopic: NodeEventsTopic, Partition: record.Partition,
			Offset: record.Offset, ErrorCode: "invalid_node_event", SourceEventID: sourceEventID},
	}
	value, err := json.Marshal(envelope)
	if err != nil {
		return nil, errors.New("ranker DLQ event serialization failed")
	}
	return &kgo.Record{Topic: deadLetterTopic, Key: []byte(eventID), Value: value, Timestamp: timestamp}, nil
}

func deterministicInvalidEventID(topic string, partition int32, offset int64) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%d", topic, partition, offset)))
	value := digest[:16]
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}
