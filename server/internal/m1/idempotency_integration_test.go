package m1

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cordova/server/internal/api"
	"cordova/server/internal/billing"
	"cordova/server/internal/domain"

	"github.com/redis/go-redis/v9"
)

type idempotencyRecoveryLedger struct {
	status billing.RequestStatus
}

func (l idempotencyRecoveryLedger) Request(_ context.Context, _ string) (billing.RequestCheckpoint, uint64, error) {
	return billing.RequestCheckpoint{Status: l.status}, 1, nil
}

func TestRedisIdempotencyClaimReplayAndRecovery(t *testing.T) {
	client := idempotencyTestRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	accountID, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	crashRequestID, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	pepper := []byte(strings.Repeat("p", 32))
	store, err := NewStore(client, Config{Origin: "https://example.test", PrivacyNoticeVersion: "v1", Pepper: pepper})
	if err != nil {
		t.Fatal(err)
	}
	key := keyRecord{ID: keyID.String(), AccountID: accountID.String(), Permissions: []string{"chat:write"},
		ModelIDs: []string{api.ModelID}}
	encoded, err := json.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	keyRedis := keyPrefix + keyID.String()
	if err := client.Set(ctx, keyRedis, encoded, 0).Err(); err != nil {
		t.Fatal(err)
	}
	principal := api.Principal{AccountID: accountID.String(), KeyID: keyID.String(), Kind: "key"}
	clientKey := "integration-idempotency-key"
	canonical := []byte(`{"messages":[{"content":"hello","role":"user"}],"model":"cordova-chat-small-v1"}`)
	base := "cv:v1:idempotency:" + accountID.String() + ":" + keyID.String() + ":" + store.secretVerifier("idempotency-key-v1\n"+accountID.String()+"\n"+keyID.String()+"\n"+clientKey)
	cleanup := []string{keyRedis, base, base + ":body", "cv:v1:idempotency-request:" + requestID.String(),
		"cv:v1:idempotency-request:" + crashRequestID.String()}
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cleanCancel()
		_ = client.SRem(cleanCtx, "cv:v1:idempotency-pending", requestID.String(), crashRequestID.String()).Err()
		_ = client.Del(cleanCtx, cleanup...).Err()
	})

	first, err := store.Claim(ctx, principal, clientKey, api.ModelID, requestID.String(), canonical)
	if err != nil || !first.Created {
		t.Fatalf("first claim=%+v err=%v", first, err)
	}
	replay, err := store.Claim(ctx, principal, clientKey, api.ModelID, crashRequestID.String(), canonical)
	if err != nil || replay.Created || replay.RequestID != requestID.String() || !replay.Compared || !replay.BodyMatch {
		t.Fatalf("in-progress replay=%+v err=%v", replay, err)
	}
	mismatch, err := store.Claim(ctx, principal, clientKey, api.ModelID, crashRequestID.String(), []byte(`{"different":true}`))
	if err != nil || mismatch.Created || mismatch.BodyMatch {
		t.Fatalf("body mismatch=%+v err=%v", mismatch, err)
	}
	if err := store.Complete(ctx, principal, clientKey, requestID.String(), "settled"); err != nil {
		t.Fatal(err)
	}
	terminal, err := store.Claim(ctx, principal, clientKey, api.ModelID, crashRequestID.String(), canonical)
	if err != nil || terminal.Status != "settled" || terminal.Created {
		t.Fatalf("terminal replay=%+v err=%v", terminal, err)
	}

	recoveryKey := "recovery-idempotency-key"
	recoveryBase := "cv:v1:idempotency:" + accountID.String() + ":" + keyID.String() + ":" + store.secretVerifier("idempotency-key-v1\n"+accountID.String()+"\n"+keyID.String()+"\n"+recoveryKey)
	cleanup = append(cleanup, recoveryBase, recoveryBase+":body")
	if claimed, err := store.Claim(ctx, principal, recoveryKey, api.ModelID, crashRequestID.String(), canonical); err != nil || !claimed.Created {
		t.Fatalf("recovery claim=%+v err=%v", claimed, err)
	}
	if err := store.ReconcileIdempotency(ctx, idempotencyRecoveryLedger{status: billing.RequestRefunded}); err != nil {
		t.Fatal(err)
	}
	refunded, err := store.Claim(ctx, principal, recoveryKey, api.ModelID, requestID.String(), canonical)
	if err != nil || refunded.Status != "refunded" || refunded.Created {
		t.Fatalf("recovered replay=%+v err=%v", refunded, err)
	}
}

func idempotencyTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("CORDOVA_TEST_REDIS_URL"))
	if raw == "" {
		t.Skip("set CORDOVA_TEST_REDIS_URL to run M1 Redis integration tests")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("invalid CORDOVA_TEST_REDIS_URL: %v", err)
	}
	if options.Network == "unix" {
		if !filepath.IsAbs(options.Addr) {
			t.Fatal("CORDOVA_TEST_REDIS_URL must use an absolute local Redis socket path")
		}
	} else {
		host, _, err := net.SplitHostPort(options.Addr)
		if err != nil || (host != "localhost" && host != "127.0.0.1" && host != "::1") {
			t.Fatal("CORDOVA_TEST_REDIS_URL must point to local Redis; the test writes temporary keys")
		}
	}
	client := redis.NewClient(options)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		t.Fatalf("local test Redis is unavailable: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}
