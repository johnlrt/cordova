package m1

import (
	"context"
	"strings"
	"testing"

	"cordova/server/internal/domain"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
)

func TestNodeEventOutboxPublishesContentFreeC7Envelope(t *testing.T) {
	client := idempotencyTestRedis(t)
	defer client.Close()
	nodeID, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	sequenceKey := nodeEventSequenceKey(nodeID.String())
	ctx := context.Background()
	previous := "0-0"
	latest, err := client.XRevRangeN(ctx, nodeEventsOutbox, "+", "-", 1).Result()
	if err != nil && err != redis.Nil {
		t.Fatal(err)
	}
	if len(latest) > 0 {
		previous = latest[0].ID
	}
	groupID, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	group := "node-event-test-" + strings.ReplaceAll(groupID.String(), "-", "")
	ids := make([]string, 0, 2)
	t.Cleanup(func() {
		_ = client.XGroupDestroy(ctx, nodeEventsOutbox, group).Err()
		if len(ids) > 0 {
			_ = client.XDel(ctx, nodeEventsOutbox, ids...).Err()
		}
		_ = client.Del(ctx, sequenceKey).Err()
	})
	service := &nodeService{redis: client}
	if err := service.appendNodeEvent(ctx, nodeID.String(), "node.join", nil, nil,
		map[string]string{"session_id": "00000000-0000-4000-8000-000000000010"}); err != nil {
		t.Fatal(err)
	}
	if err := client.XGroupCreateMkStream(ctx, nodeEventsOutbox, group, previous).Err(); err != nil {
		t.Fatal(err)
	}
	streams, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: group, Consumer: "test-consumer", Streams: []string{nodeEventsOutbox, ">"}, Count: 16,
	}).Result()
	if err != nil || len(streams) != 1 {
		t.Fatalf("read node event outbox = %#v, %v", streams, err)
	}
	var record *kgo.Record
	for _, message := range streams[0].Messages {
		if value, ok := message.Values["node_id"].(string); ok && value == nodeID.String() {
			ids = append(ids, message.ID)
			record, err = nodeEventKafkaRecord(message)
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if record == nil || record.Topic != "cordova.node.events.v1" || string(record.Key) != nodeID.String() ||
		strings.Contains(string(record.Value), "prompt") || strings.Contains(string(record.Value), "response") {
		t.Fatalf("published node event is invalid or contains content: %#v", record)
	}
	if sequence, err := client.Get(ctx, sequenceKey).Result(); err != nil || sequence != "1" {
		t.Fatalf("node outbox sequence = %q, %v; want 1", sequence, err)
	}
}
