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

func rankOutcomeMessage(id, eventID, requestID, attemptID string, payload RequestOutcomePayload) redis.XMessage {
	message := rankMessage(id, "node.request_outcome", payload)
	message.Values["event_id"] = eventID
	message.Values["request_id"] = requestID
	message.Values["attempt_id"] = attemptID
	return message
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

func TestRankAggregationBuildsContentFreeT01AndT03ReviewSignals(t *testing.T) {
	aggregate := newRankAggregation(time.Now().UTC())
	passed := true
	benchmark := BenchmarkPayload{ModelID: "cordova-chat-small-v1", VerifiedOutputTokens: 400,
		VerifiedSlots: 2, ElapsedMS: 4_000, Passed: &passed}
	if err := aggregate.add(rankMessage("1791720000000-0", "node.benchmark", benchmark)); err != nil {
		t.Fatal(err)
	}
	outcome := RequestOutcomePayload{ModelID: "cordova-chat-small-v1", Outcome: "success", ElapsedMS: 1_000,
		VerifiedOutputTokens: 121, ClaimDeviation: true}
	eventID := "00000000-0000-4000-8000-000000000020"
	requestID := "00000000-0000-4000-8000-000000000021"
	attemptID := "00000000-0000-4000-8000-000000000022"
	if err := aggregate.add(rankOutcomeMessage("1791720001000-0", eventID, requestID, attemptID, outcome)); err != nil {
		t.Fatal(err)
	}
	if len(aggregate.fraudSignals) != 2 {
		t.Fatalf("fraud signals = %+v; want T01 and T03 flags", aggregate.fraudSignals)
	}
	want := map[string]bool{fraudClaimDeviation: false, fraudHighThroughput: false}
	for _, signal := range aggregate.fraudSignals {
		if signal.SourceEventID != eventID || signal.RequestID != requestID || signal.AttemptID != attemptID || signal.ObservedAt == "" {
			t.Fatalf("fraud signal contains incorrect event metadata: %+v", signal)
		}
		if _, ok := want[signal.Code]; !ok {
			t.Fatalf("unexpected fraud signal code %q", signal.Code)
		}
		want[signal.Code] = true
	}
	for code, seen := range want {
		if !seen {
			t.Fatalf("missing fraud signal %q", code)
		}
	}
}

func TestRankAggregationBuildsT02CanaryReviewSignalWithoutRequestIDs(t *testing.T) {
	aggregate := newRankAggregation(time.Now().UTC())
	canary := CanaryPayload{CanaryID: "00000000-0000-4000-8000-000000000050", ModelID: "cordova-chat-small-v1",
		CanaryVersion: "arith-json-v1", Passed: false, FailureCode: "property_mismatch"}
	message := rankMessage("1791720003000-0", "node.canary", canary)
	message.Values["event_id"] = "00000000-0000-4000-8000-000000000051"
	message.Values["request_id"], message.Values["attempt_id"] = "", ""
	if err := aggregate.add(message); err != nil {
		t.Fatal(err)
	}
	if len(aggregate.fraudSignals) != 1 {
		t.Fatalf("canary fraud signals = %+v", aggregate.fraudSignals)
	}
	signal := aggregate.fraudSignals[0]
	if signal.Code != fraudCanaryFailure || signal.CanaryID != canary.CanaryID || signal.RequestID != "" || signal.AttemptID != "" ||
		signal.ModelID != canary.ModelID || signal.CanaryVersion != canary.CanaryVersion || signal.DetailCode != canary.FailureCode {
		t.Fatalf("canary signal = %+v", signal)
	}
}

func uint32Pointer(value uint32) *uint32 { return &value }
