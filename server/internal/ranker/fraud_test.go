package ranker

import "testing"

func TestT03ThroughputSignalsUsePerSlotBenchmarkAndExactBoundaries(t *testing.T) {
	passed := true
	benchmark := BenchmarkPayload{ModelID: "model-a", VerifiedOutputTokens: 400, VerifiedSlots: 2, ElapsedMS: 4_000, Passed: &passed}
	tests := []struct {
		name     string
		outcome  RequestOutcomePayload
		wantCode string
	}{
		{name: "fast allowance boundary", outcome: RequestOutcomePayload{ModelID: "model-a", ElapsedMS: 1_000, VerifiedOutputTokens: 120}},
		{name: "fast anomaly above allowance", outcome: RequestOutcomePayload{ModelID: "model-a", ElapsedMS: 1_000, VerifiedOutputTokens: 121}, wantCode: fraudHighThroughput},
		{name: "low anomaly", outcome: RequestOutcomePayload{ModelID: "model-a", ElapsedMS: 10_000, VerifiedOutputTokens: 99}, wantCode: fraudLowThroughput},
		{name: "low anomaly threshold boundary", outcome: RequestOutcomePayload{ModelID: "model-a", ElapsedMS: 10_000, VerifiedOutputTokens: 100}},
		{name: "short result excluded from low signal", outcome: RequestOutcomePayload{ModelID: "model-a", ElapsedMS: 10_000, VerifiedOutputTokens: 31}},
		{name: "short elapsed excluded from low signal", outcome: RequestOutcomePayload{ModelID: "model-a", ElapsedMS: 9_999, VerifiedOutputTokens: 32}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := throughputFraudSignal(test.outcome, benchmark); got != test.wantCode {
				t.Fatalf("throughput fraud signal = %q, want %q", got, test.wantCode)
			}
		})
	}
	failed := false
	benchmark.Passed = &failed
	if got := throughputFraudSignal(RequestOutcomePayload{ElapsedMS: 1_000, VerifiedOutputTokens: 10_000}, benchmark); got != "" {
		t.Fatalf("failed benchmark generated signal %q", got)
	}
}

func TestFraudSignalsRequireSameModelBenchmark(t *testing.T) {
	passed := true
	benchmark := BenchmarkPayload{ModelID: "model-a", VerifiedOutputTokens: 100, VerifiedSlots: 1, ElapsedMS: 1_000, Passed: &passed}
	if got := throughputFraudSignal(RequestOutcomePayload{ModelID: "model-b", ElapsedMS: 1_000, VerifiedOutputTokens: 1_000}, benchmark); got != "" {
		t.Fatalf("different-model benchmark generated signal %q", got)
	}
	if got := throughputFraudSignal(RequestOutcomePayload{ModelID: "model-a", ElapsedMS: 1_000, VerifiedOutputTokens: 1_000}, benchmark); got != fraudHighThroughput {
		t.Fatalf("matching-model throughput signal = %q", got)
	}
}
