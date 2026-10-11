package ranker

import (
	"math/big"
)

const (
	fraudCanaryFailure  = "t02_canary_failure"
	fraudClaimDeviation = "t01_claim_deviation"
	fraudHighThroughput = "t03_high_throughput"
	fraudLowThroughput  = "t03_low_throughput"
)

type fraudSignal struct {
	SourceEventID string
	RequestID     string
	AttemptID     string
	CanaryID      string
	ModelID       string
	CanaryVersion string
	DetailCode    string
	Code          string
	ObservedAt    string
}

func validCanaryFailureCode(value string) bool {
	switch value {
	case "invalid_format", "nonce_mismatch", "property_mismatch", "runner_timeout", "runner_error", "tokenizer_error":
		return true
	default:
		return false
	}
}

// throughputFraudSignal compares one server-measured request with the
// benchmark for the same model. Benchmark throughput is averaged per verified
// slot so a concurrent capacity challenge is comparable to one request.
func throughputFraudSignal(outcome RequestOutcomePayload, benchmark BenchmarkPayload) string {
	if outcome.ModelID == "" || outcome.ModelID != benchmark.ModelID || benchmark.Passed == nil || !*benchmark.Passed || benchmark.VerifiedOutputTokens == 0 ||
		benchmark.VerifiedSlots == 0 || benchmark.ElapsedMS == 0 || outcome.ElapsedMS == 0 {
		return ""
	}
	actual := new(big.Int).SetUint64(uint64(outcome.VerifiedOutputTokens))
	benchElapsed := new(big.Int).SetUint64(uint64(benchmark.ElapsedMS))
	requestElapsed := new(big.Int).SetUint64(uint64(outcome.ElapsedMS))
	benchmarkTokens := new(big.Int).SetUint64(uint64(benchmark.VerifiedOutputTokens))
	slots := new(big.Int).SetUint64(uint64(benchmark.VerifiedSlots))

	// T03 fast signal: actual > 2x per-slot benchmark rate plus 20 tokens.
	fastLeft := new(big.Int).Mul(new(big.Int).Set(actual), benchElapsed)
	fastLeft.Mul(fastLeft, slots)
	fastRight := new(big.Int).Mul(new(big.Int).Set(benchmarkTokens), requestElapsed)
	fastRight.Mul(fastRight, big.NewInt(2))
	allowance := new(big.Int).Mul(new(big.Int).Set(benchElapsed), slots)
	allowance.Mul(allowance, big.NewInt(20))
	fastRight.Add(fastRight, allowance)
	if fastLeft.Cmp(fastRight) > 0 {
		return fraudHighThroughput
	}

	if outcome.VerifiedOutputTokens < 32 || outcome.ElapsedMS < 10_000 {
		return ""
	}
	// T03 low signal: less than 20% of per-slot benchmark throughput.
	slowLeft := new(big.Int).Mul(new(big.Int).Set(actual), benchElapsed)
	slowLeft.Mul(slowLeft, slots)
	slowLeft.Mul(slowLeft, big.NewInt(5))
	slowRight := new(big.Int).Mul(new(big.Int).Set(benchmarkTokens), requestElapsed)
	if slowLeft.Cmp(slowRight) < 0 {
		return fraudLowThroughput
	}
	return ""
}
