package m1

import (
	"context"
	"testing"

	"cordova/server/internal/domain"
)

func TestRoutingRankReadsProjectionAndFencesQuarantine(t *testing.T) {
	client := idempotencyTestRedis(t)
	service := &nodeService{redis: client}
	nodeID, err := domain.NewID()
	if err != nil {
		t.Fatal(err)
	}
	rankKey := "cv:v1:rank:" + nodeID.String()
	quarantineKey := "cv:v1:node-quarantine:" + nodeID.String()
	t.Cleanup(func() { _ = client.Del(context.Background(), rankKey, quarantineKey).Err() })
	ctx := context.Background()
	if rank, err := service.routingRank(ctx, nodeID.String(), 10); err != nil || rank != 10 {
		t.Fatalf("missing projection rank = %d, %v; want provisional fallback 10", rank, err)
	}
	if err := client.HSet(ctx, rankKey, "rank", "73").Err(); err != nil {
		t.Fatal(err)
	}
	if rank, err := service.routingRank(ctx, nodeID.String(), 10); err != nil || rank != 73 {
		t.Fatalf("projected rank = %d, %v; want 73", rank, err)
	}
	if err := client.Set(ctx, quarantineKey, "1", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if rank, err := service.routingRank(ctx, nodeID.String(), 10); err != nil || rank != 0 {
		t.Fatalf("quarantined rank = %d, %v; want 0", rank, err)
	}
}
