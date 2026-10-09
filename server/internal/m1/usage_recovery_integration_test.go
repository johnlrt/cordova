package m1

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cordova/server/internal/api"
	"cordova/server/internal/billing"
	"cordova/server/internal/domain"

	"github.com/redis/go-redis/v9"
)

type pendingUsageRecoverySource struct {
	requestID  string
	checkpoint billing.RequestCheckpoint
	operation  billing.Entry
}

func (s pendingUsageRecoverySource) PendingUsageRequestIDs(context.Context) ([]string, error) {
	return []string{s.requestID}, nil
}

func (s pendingUsageRecoverySource) Request(_ context.Context, requestID string) (billing.RequestCheckpoint, uint64, error) {
	if requestID != s.requestID {
		return billing.RequestCheckpoint{}, 0, billing.ErrRequestNotFound
	}
	return s.checkpoint, 1, nil
}

func (s pendingUsageRecoverySource) Operation(_ context.Context, operationID string) (billing.Entry, error) {
	if operationID != s.operation.OperationID {
		return billing.Entry{}, billing.ErrRequestNotFound
	}
	return s.operation, nil
}

func TestRedisUsageRecoveryReplaysTerminalProjectionWithoutContent(t *testing.T) {
	client := idempotencyTestRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	accountID := integrationID(t)
	keyID := integrationID(t)
	requestID := integrationID(t)
	attemptID := integrationID(t)
	ownerID := integrationID(t)
	version, _ := domain.ParseID("00000000-0000-4000-8000-000000000001")
	zero, _ := domain.NewPiconero(0)
	acceptedAt := time.Now().UTC().Add(-20 * time.Second)
	finishedAt := acceptedAt.Add(11 * time.Second)
	pendingKey := "cv:test:m1:" + requestID + ":usage-pending"
	store, err := NewStore(client, Config{Origin: "https://example.test", PrivacyNoticeVersion: "v1",
		Pepper: []byte(strings.Repeat("p", 32)), UsagePendingKey: pendingKey})
	if err != nil {
		t.Fatal(err)
	}
	key := keyRecord{ID: keyID, AccountID: accountID, Permissions: []string{"chat:write"}, ModelIDs: []string{api.ModelID}}
	encodedKey, err := json.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	keyRedis := keyPrefix + keyID
	if err := client.Set(ctx, keyRedis, encodedKey, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.SAdd(ctx, pendingKey, requestID).Err(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cleanupCancel()
		_ = client.Del(cleanupCtx, keyRedis, pendingKey).Err()
	}()

	checkpoint := billing.RequestCheckpoint{AccountID: accountID, APIKeyID: keyID, ModelID: api.ModelID, OwnerID: ownerID,
		InputTokens: 3, MaxOutputTokens: 8, AcceptedAt: acceptedAt, Price: billing.Price{Version: version, InputRate: zero, OutputRate: zero},
		Status: billing.RequestSettled, Attempts: map[string]billing.AttemptCheckpoint{attemptID: {
			Reservation: billing.Reservation{RequestID: requestID, AttemptID: attemptID, ModelID: api.ModelID,
				AccountID: accountID, APIKeyID: keyID, OwnerID: ownerID, Status: billing.AttemptSettled}, OutputTokens: 2,
		}},
	}
	ledger := pendingUsageRecoverySource{requestID: requestID, checkpoint: checkpoint,
		operation: billing.Entry{OperationID: "settle:" + requestID, CreatedAt: finishedAt}}
	if err := store.ReconcileAccountedUsage(ctx, ledger); err != nil {
		t.Fatal(err)
	}
	if pending, err := client.SCard(ctx, pendingKey).Result(); err != nil || pending != 0 {
		t.Fatalf("terminal usage recovery queue remains: count=%d err=%v", pending, err)
	}
	entries, err := client.XRevRangeN(ctx, "cv:v1:outbox:usage", "+", "-", 20).Result()
	if err != nil {
		t.Fatal(err)
	}
	var streamID string
	found := false
	for _, entry := range entries {
		payloadText, ok := entry.Values["payload"].(string)
		if !ok || !strings.Contains(payloadText, requestID) {
			continue
		}
		var event usageEvent
		if err := json.Unmarshal([]byte(payloadText), &event); err != nil {
			t.Fatal(err)
		}
		if event.RequestID == nil || *event.RequestID != requestID || event.Payload.Status != "settled" ||
			event.Payload.InputTokens != 3 || event.Payload.OutputTokens != 2 || event.Payload.Charged != "0" ||
			event.Payload.LatencyMS != 11_000 || strings.Contains(payloadText, "prompt sentinel") {
			t.Fatalf("recovered usage projection is invalid or contains content: %s", payloadText)
		}
		streamID, found = entry.ID, true
		break
	}
	if !found {
		t.Fatal("recovered usage projection was not written to the outbox")
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cleanupCancel()
		_ = client.XDel(cleanupCtx, "cv:v1:outbox:usage", streamID).Err()
	})
}

func TestRedisLiveCursorReceivesEventsCreatedAfterHandshake(t *testing.T) {
	client := idempotencyTestRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	accountID := integrationID(t)
	resourceID := integrationID(t)
	stream := "cv:v1:live:" + accountID
	t.Cleanup(func() { _ = client.Del(context.Background(), stream).Err() })
	store, err := NewStore(client, Config{Origin: "https://example.test", PrivacyNoticeVersion: "v1", Pepper: []byte(strings.Repeat("p", 32))})
	if err != nil {
		t.Fatal(err)
	}
	_, cursor, err := store.LiveEvents(ctx, accountID, "")
	if err != nil || cursor != "0-0" {
		t.Fatalf("initial live cursor=%q err=%v", cursor, err)
	}
	eventID, err := client.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: map[string]any{
		"topic": "usage", "resource_id": resourceID, "revision": "1", "action": "invalidate",
	}}).Result()
	if err != nil {
		t.Fatal(err)
	}
	events, next, err := store.LiveEvents(ctx, accountID, cursor)
	if err != nil || len(events) != 1 || events[0].EventID != eventID || events[0].ResourceID != resourceID || next != eventID {
		t.Fatalf("events after initial subscription cursor=%+v next=%q err=%v", events, next, err)
	}
}

func integrationID(t *testing.T) string {
	t.Helper()
	id, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}
