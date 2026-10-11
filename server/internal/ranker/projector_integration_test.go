package ranker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"cordova/server/internal/domain"
	"github.com/redis/go-redis/v9"
)

func TestRedisProjectorAtomicallyDeduplicatesAndFencesSessionSequence(t *testing.T) {
	client := rankerIntegrationRedis(t)
	defer client.Close()
	node, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	group := "ranker-test-" + fmt.Sprintf("%d", time.Now().UnixNano())
	partition := int32(5)
	eventIDs := make([]string, 4)
	for index := range eventIDs {
		id, err := domain.NewID()
		if err != nil {
			t.Fatal(err)
		}
		eventIDs[index] = id.String()
	}
	firstSessionID := "00000000-0000-4000-8000-000000000010"
	secondSessionID := "00000000-0000-4000-8000-000000000011"
	nodeID := node.String()
	streamKey := "cv:v1:rank-events:" + nodeID
	offsetKey := fmt.Sprintf("cv:v1:consumer-offset:%s:%s:%d", group, NodeEventsTopic, partition)
	sequenceKey := "cv:v1:rank-sequence:" + nodeID
	sessionKey := "cv:v1:ranker-session:" + nodeID
	nodesKey := "cv:v1:ranker-nodes"
	seenKeys := make([]string, 0, len(eventIDs))
	for _, eventID := range eventIDs {
		seenKeys = append(seenKeys, "cv:v1:ranker-event:"+eventID)
	}
	allKeys := append([]string{streamKey, offsetKey, sequenceKey, sessionKey}, seenKeys...)
	if err := client.Del(context.Background(), allKeys...).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.SRem(context.Background(), nodesKey, nodeID).Err()
		_ = client.Del(context.Background(), allKeys...).Err()
	})

	firstEvent := testEventWithID(nodeID, eventIDs[0], "node.join", fmt.Sprintf(`{"session_id":%q}`, firstSessionID), "null", "null", 101)
	status, err := Apply(context.Background(), client, group, partition, 20, firstEvent)
	if err != nil || status != EventInserted {
		t.Fatalf("first Apply = %v, %v", status, err)
	}
	status, err = Apply(context.Background(), client, group, partition, 20, firstEvent)
	if err != nil || status != OffsetAlreadyApplied {
		t.Fatalf("offset replay = %v, %v", status, err)
	}
	status, err = Apply(context.Background(), client, group, partition, 21, firstEvent)
	if err != nil || status != DuplicateEvent {
		t.Fatalf("event replay = %v, %v", status, err)
	}

	staleSequence := testEventWithID(nodeID, eventIDs[1], "node.leave",
		fmt.Sprintf(`{"session_id":%q,"reason":"session_replaced"}`, firstSessionID), "null", "null", 100)
	status, err = Apply(context.Background(), client, group, partition, 22, staleSequence)
	if err != nil || status != StaleSequence {
		t.Fatalf("stale sequence = %v, %v", status, err)
	}

	newSession := testEventWithID(nodeID, eventIDs[2], "node.join", fmt.Sprintf(`{"session_id":%q}`, secondSessionID), "null", "null", 103)
	status, err = Apply(context.Background(), client, group, partition, 23, newSession)
	if err != nil || status != EventInserted {
		t.Fatalf("new session = %v, %v", status, err)
	}
	oldLeave := testEventWithID(nodeID, eventIDs[3], "node.leave",
		fmt.Sprintf(`{"session_id":%q,"reason":"late_old_session"}`, firstSessionID), "null", "null", 104)
	status, err = Apply(context.Background(), client, group, partition, 24, oldLeave)
	if err != nil || status != StaleSession {
		t.Fatalf("old session leave = %v, %v", status, err)
	}
	heartbeatID, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	allKeys = append(allKeys, "cv:v1:ranker-event:"+heartbeatID.String())
	heartbeat := testEventWithID(nodeID, heartbeatID.String(), "node.heartbeat",
		fmt.Sprintf(`{"session_id":%q,"latency_ms":2,"model_states":[]}`, secondSessionID), "null", "null", 105)
	status, err = Apply(context.Background(), client, group, partition, 25, heartbeat)
	if err != nil || status != EventInserted {
		t.Fatalf("current session heartbeat = %v, %v", status, err)
	}
	if count, err := client.XLen(context.Background(), streamKey).Result(); err != nil || count != 3 {
		t.Fatalf("projected stream length = %d, %v; want 3", count, err)
	}
	if sequence, err := client.Get(context.Background(), sequenceKey).Result(); err != nil || sequence != fmt.Sprintf("%020d", 105) {
		t.Fatalf("sequence = %q, %v", sequence, err)
	}
	if offset, err := client.Get(context.Background(), offsetKey).Result(); err != nil || offset != fmt.Sprintf("%020d", 25) {
		t.Fatalf("offset = %q, %v", offset, err)
	}
	if session, err := client.Get(context.Background(), sessionKey).Result(); err != nil || session != secondSessionID {
		t.Fatalf("current session = %q, %v", session, err)
	}
	if member, err := client.SIsMember(context.Background(), nodesKey, nodeID).Result(); err != nil || !member {
		t.Fatalf("ranker node index contains node = %v, %v", member, err)
	}
}

func TestRedisRankPersistenceAndOutboxAreAtomicAndReplayable(t *testing.T) {
	client := rankerIntegrationRedis(t)
	defer client.Close()
	ctx := context.Background()
	node, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	rankKey := "cv:v1:rank:" + node.String()
	outboxKey := "cv:v1:test:rank-outbox:" + node.String()
	group := "rank-outbox-test-" + strings.ReplaceAll(node.String(), "-", "")
	if err := client.Del(ctx, rankKey, outboxKey).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.XGroupDestroy(ctx, outboxKey, group).Err()
		_ = client.Del(ctx, rankKey, outboxKey).Err()
	})
	firstID, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	updated := time.Now().UTC().Format(time.RFC3339Nano)
	apply := func(rank int, eventID string) int64 {
		result, err := client.Eval(ctx, persistRankScript, []string{rankKey, outboxKey}, rank,
			FormulaVersion, false, updated, eventID, node.String(), "4294967296", "50", "120", "0.5", "0.9").Int64()
		if err != nil {
			t.Fatalf("persist rank: %v", err)
		}
		return result
	}
	if status := apply(56, firstID.String()); status != 1 {
		t.Fatalf("initial rank change status = %d, want 1", status)
	}
	secondID, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if status := apply(56, secondID.String()); status != 0 {
		t.Fatalf("unchanged rank status = %d, want 0", status)
	}
	if length, err := client.XLen(ctx, outboxKey).Result(); err != nil || length != 1 {
		t.Fatalf("rank outbox length = %d, %v; want one change", length, err)
	}
	if rank, err := client.HGet(ctx, rankKey, "rank").Result(); err != nil || rank != "56" {
		t.Fatalf("stored rank = %q, %v", rank, err)
	}
	if err := client.XGroupCreateMkStream(ctx, outboxKey, group, "0").Err(); err != nil {
		t.Fatal(err)
	}
	streams, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: group, Consumer: "test-consumer", Streams: []string{outboxKey, ">"}, Count: 1,
	}).Result()
	if err != nil || len(streams) != 1 || len(streams[0].Messages) != 1 {
		t.Fatalf("read rank outbox = %#v, %v", streams, err)
	}
	message := streams[0].Messages[0]
	record, err := rankOutboxKafkaRecord(message)
	if err != nil {
		t.Fatal(err)
	}
	var envelope rankChangedEnvelope
	if record.Topic != "cordova.node.rank.v1" || string(record.Key) != node.String() || json.Unmarshal(record.Value, &envelope) != nil ||
		envelope.EventID != firstID.String() || envelope.Payload.Rank != 56 || envelope.Sequence != 1 {
		t.Fatalf("rank Kafka record = topic %q key %q envelope %+v", record.Topic, record.Key, envelope)
	}
	if err := client.Eval(ctx, acknowledgeRankOutboxScript, []string{outboxKey}, group, message.ID).Err(); err != nil {
		t.Fatal(err)
	}
	if length, err := client.XLen(ctx, outboxKey).Result(); err != nil || length != 0 {
		t.Fatalf("acknowledged rank outbox length = %d, %v; want 0", length, err)
	}
}

func rankerIntegrationRedis(t *testing.T) *redis.Client {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("CORDOVA_TEST_REDIS_URL"))
	if raw == "" {
		t.Skip("set CORDOVA_TEST_REDIS_URL to run ranker Redis projection tests")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("invalid CORDOVA_TEST_REDIS_URL: %v", err)
	}
	if options.Network == "tcp" {
		host, _, splitErr := net.SplitHostPort(options.Addr)
		if splitErr != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
			t.Fatal("CORDOVA_TEST_REDIS_URL must use local Redis; tests only write isolated keys")
		}
	} else if options.Network != "unix" {
		t.Fatal("CORDOVA_TEST_REDIS_URL must use a local TCP or Unix socket")
	}
	client := redis.NewClient(options)
	if err := client.Ping(context.Background()).Err(); err != nil {
		client.Close()
		t.Fatalf("connect to test Redis: %v", err)
	}
	return client
}
