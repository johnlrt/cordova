// Package ranker implements the versioned C6 node ranking formula.
//
// It is deliberately a pure calculation: callers provide server-observed
// window aggregates, and only the ranker may publish the resulting rank.
package ranker

import (
	"errors"
	"math/big"
	"time"
)

const (
	FormulaVersion                 = "c6-v1"
	provisionalRank                = 10
	benchmarkMaxAge                = 24 * time.Hour
	capacityLimitBytes      uint64 = 24 * 1024 * 1024 * 1024
	latencyLimitMillis      uint64 = 2_000
	minimumFinishedAttempts uint64 = 10
)

var ErrInvalidWindow = errors.New("invalid rank window")

// Window contains server-observed aggregates for one node over its rank
// window. Benchmark throughput is represented as verified output tokens and
// elapsed milliseconds so the formula can preserve exact fractional values.
// P50 latency and heartbeat counts must use server-observed timestamps.
type Window struct {
	Quarantined                 bool
	BenchmarkPassed             bool
	BenchmarkAt                 time.Time
	Now                         time.Time
	TestedCapacityBytes         uint64
	VerifiedOutputTokens        uint64
	BenchmarkElapsedMS          uint64
	ConnectedHeartbeatIntervals uint64
	ExpectedHeartbeatIntervals  uint64
	P50ServerLatencyMillis      uint64
	LatencySamples              uint64
	SuccessfulAttempts          uint64
	FinishedAttempts            uint64
}

type Result struct {
	Rank           int
	FormulaVersion string
	Provisional    bool
}

// Compute applies C6's score exactly and rounds down only once at the end.
// Before a passing benchmark and ten finished outcomes, nodes retain the
// provisional rank 10. Quarantine always removes a node from routing with 0.
func Compute(window Window) (Result, error) {
	result := Result{FormulaVersion: FormulaVersion}
	if window.SuccessfulAttempts > window.FinishedAttempts || window.LatencySamples > window.FinishedAttempts {
		return Result{}, ErrInvalidWindow
	}
	if window.Quarantined {
		return result, nil
	}
	if !window.BenchmarkAt.IsZero() && !window.Now.IsZero() && window.BenchmarkAt.After(window.Now) {
		return Result{}, ErrInvalidWindow
	}
	if !window.BenchmarkPassed || window.BenchmarkAt.IsZero() || window.Now.IsZero() ||
		window.Now.Sub(window.BenchmarkAt) > benchmarkMaxAge || window.FinishedAttempts < minimumFinishedAttempts ||
		window.LatencySamples == 0 {
		result.Rank = provisionalRank
		result.Provisional = true
		return result, nil
	}
	if window.ExpectedHeartbeatIntervals == 0 || window.TestedCapacityBytes == 0 {
		return Result{}, ErrInvalidWindow
	}
	if window.BenchmarkElapsedMS == 0 {
		return Result{}, ErrInvalidWindow
	}

	capacityNumerator := min(window.TestedCapacityBytes, capacityLimitBytes)
	heartbeatsNumerator := min(window.ConnectedHeartbeatIntervals, window.ExpectedHeartbeatIntervals)
	latencyNumerator := uint64(0)
	if window.P50ServerLatencyMillis < latencyLimitMillis {
		latencyNumerator = latencyLimitMillis - window.P50ServerLatencyMillis
	}

	total := new(big.Rat)
	total.Add(total, weightedFraction(20, capacityNumerator, capacityLimitBytes))
	total.Add(total, weightedThroughputFraction(30, window.VerifiedOutputTokens, window.BenchmarkElapsedMS))
	total.Add(total, weightedFraction(20, heartbeatsNumerator, window.ExpectedHeartbeatIntervals))
	total.Add(total, weightedFraction(10, latencyNumerator, latencyLimitMillis))

	successes := new(big.Int).SetUint64(window.SuccessfulAttempts)
	successes.Add(successes, big.NewInt(1))
	finished := new(big.Int).SetUint64(window.FinishedAttempts)
	finished.Add(finished, big.NewInt(2))
	successRate := new(big.Rat).SetFrac(successes, finished)
	successRate.Mul(successRate, big.NewRat(20, 1))
	total.Add(total, successRate)

	rank := new(big.Int).Quo(total.Num(), total.Denom())
	result.Rank = int(rank.Int64())
	if result.Rank < 0 {
		result.Rank = 0
	} else if result.Rank > 100 {
		result.Rank = 100
	}
	return result, nil
}

// Throughput is tokens / (elapsed_ms / 1000), normalized to 100 tokens/s.
// Therefore T = min(tokens * 10 / elapsed_ms, 1). Use big integers so callers
// cannot overflow the multiplication even with adversarial aggregate values.
func weightedThroughputFraction(weight int64, tokens, elapsedMS uint64) *big.Rat {
	numerator := new(big.Int).SetUint64(tokens)
	numerator.Mul(numerator, big.NewInt(10))
	denominator := new(big.Int).SetUint64(elapsedMS)
	if numerator.Cmp(denominator) >= 0 {
		return big.NewRat(weight, 1)
	}
	return new(big.Rat).SetFrac(numerator.Mul(numerator, big.NewInt(weight)), denominator)
}

func weightedFraction(weight int64, numerator, denominator uint64) *big.Rat {
	n := new(big.Int).SetUint64(numerator)
	n.Mul(n, big.NewInt(weight))
	d := new(big.Int).SetUint64(denominator)
	return new(big.Rat).SetFrac(n, d)
}
