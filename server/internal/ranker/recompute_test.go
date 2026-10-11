package ranker

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func rankMessage(id, eventType string, payload any) redis.XMessage {
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return redis.XMessage{ID: id, Values: map[string]any{"event_type": eventType, "payload": string(encoded)}}
}

func TestRankAggregationUsesRedisArrivalTimeAndDeduplicatesHeartbeatIntervals(t *testing.T) {
	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	aggregate := newRankAggregation(base.Add(time.Hour))
	passed := true
	events := []redis.XMessage{
		rankMessage("1791720000000-0", "node.heartbeat", HeartbeatPayload{SessionID: "00000000-0000-4000-8000-000000000001"}),
		rankMessage("1791720005000-0", "node.heartbeat", HeartbeatPayload{SessionID: "00000000-0000-4000-8000-000000000001"}),
		rankMessage("1791720016000-0", "node.heartbeat", HeartbeatPayload{SessionID: "00000000-0000-4000-8000-000000000001"}),
		rankMessage("1791720020000-0", "node.benchmark", BenchmarkPayload{
			TestedCapacityBytes: 1024, VerifiedOutputTokens: 100, ElapsedMS: 2_000, Passed: &passed,
		}),
		rankMessage("1791720021000-0", "node.request_outcome", RequestOutcomePayload{
			Outcome: "success", FirstTokenMS: uint32Pointer(120),
		}),
		rankMessage("1791720022000-0", "node.request_outcome", RequestOutcomePayload{
			Outcome: "failure", FirstTokenMS: uint32Pointer(240),
		}),
	}
	for _, event := range events {
		if err := aggregate.add(event); err != nil {
			t.Fatal(err)
		}
	}
	window := aggregate.finish()
	if window.ConnectedHeartbeatIntervals != 2 {
		t.Fatalf("connected intervals = %d, want 2 deduplicated 15-second slots", window.ConnectedHeartbeatIntervals)
	}
	if !window.BenchmarkPassed || window.BenchmarkAt.UnixMilli() != 1791720020000 ||
		window.VerifiedOutputTokens != 100 || window.BenchmarkElapsedMS != 2_000 {
		t.Fatalf("benchmark window = %+v", window)
	}
	if window.FinishedAttempts != 2 || window.SuccessfulAttempts != 1 || window.LatencySamples != 2 ||
		window.P50ServerLatencyMillis != 120 {
		t.Fatalf("outcome window = %+v; expected nearest-rank lower p50", window)
	}
}

func TestLatestFailedBenchmarkInvalidatesEarlierPassingBenchmark(t *testing.T) {
	aggregate := newRankAggregation(time.Now().UTC())
	passed, failed := true, false
	for _, event := range []redis.XMessage{
		rankMessage("1791720000000-0", "node.benchmark", BenchmarkPayload{
			TestedCapacityBytes: 1024, VerifiedOutputTokens: 100, ElapsedMS: 2_000, Passed: &passed,
		}),
		rankMessage("1791720001000-0", "node.benchmark", BenchmarkPayload{
			TestedCapacityBytes: 1024, VerifiedOutputTokens: 100, ElapsedMS: 2_000, Passed: &failed,
		}),
	} {
		if err := aggregate.add(event); err != nil {
			t.Fatal(err)
		}
	}
	window := aggregate.finish()
	if window.BenchmarkPassed {
		t.Fatalf("latest failed benchmark must replace prior pass: %+v", window)
	}
}

func uint32Pointer(value uint32) *uint32 { return &value }
