package m1

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"

	"cordova/server/internal/domain"
)

func TestNodeModelPreparationPermitLimitIsSharedAcrossServices(t *testing.T) {
	client := idempotencyTestRedis(t)
	signingKey := ed25519.NewKeyFromSeed([]byte(strings.Repeat("p", ed25519.SeedSize)))
	config := Config{
		Origin:                  "https://nodes.example.test",
		Pepper:                  []byte(strings.Repeat("p", 32)),
		NodeSecretEncryptionKey: []byte(strings.Repeat("e", 32)),
		NodeJWTSigningKey:       signingKey,
	}
	first, err := newNodeService(client, config)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newNodeService(client, config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	permitIDs := make([]string, 3)
	for index := range permitIDs {
		id, idErr := domain.NewID()
		if idErr != nil {
			t.Fatal(idErr)
		}
		permitIDs[index] = id.String()
	}
	t.Cleanup(func() {
		for _, permitID := range permitIDs {
			_ = first.releaseNodeModelPreparationPermit(context.Background(), permitID)
		}
	})

	if err := first.acquireNodeModelPreparationPermit(ctx, permitIDs[0]); err != nil {
		t.Fatalf("first service acquire permit 1: %v", err)
	}
	if err := second.acquireNodeModelPreparationPermit(ctx, permitIDs[1]); err != nil {
		t.Fatalf("second service acquire permit 2: %v", err)
	}
	if err := first.acquireNodeModelPreparationPermit(ctx, permitIDs[2]); !errors.Is(err, ErrNodePreparationActive) {
		t.Fatalf("third permit error=%v, want global preparation limit", err)
	}
	if err := first.releaseNodeModelPreparationPermit(ctx, permitIDs[0]); err != nil {
		t.Fatalf("release permit 1: %v", err)
	}
	if err := second.acquireNodeModelPreparationPermit(ctx, permitIDs[2]); err != nil {
		t.Fatalf("acquire permit after release: %v", err)
	}
}
