package ranker

import (
	"errors"
	"testing"
	"time"
)

func readyWindow(now time.Time) Window {
	return Window{
		BenchmarkPassed:             true,
		BenchmarkAt:                 now.Add(-time.Hour),
		Now:                         now,
		TestedCapacityBytes:         12 * 1024 * 1024 * 1024,
		VerifiedOutputTokens:        250,
		BenchmarkElapsedMS:          5_000,
		ConnectedHeartbeatIntervals: 50,
		ExpectedHeartbeatIntervals:  100,
		P50ServerLatencyMillis:      1_000,
		LatencySamples:              10,
		SuccessfulAttempts:          9,
		FinishedAttempts:            10,
	}
}

func TestComputeAppliesVersionedWeightedFormulaAndFloorsOnce(t *testing.T) {
	window := readyWindow(time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	result, err := Compute(window)
	if err != nil {
		t.Fatal(err)
	}
	if result.Rank != 56 || result.FormulaVersion != FormulaVersion || result.Provisional {
		t.Fatalf("rank result = %+v, want exact v1 score 56", result)
	}
}

func TestComputeUsesProvisionalRankUntilEvidenceIsReady(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	base := readyWindow(now)
	tests := map[string]func(*Window){
		"benchmark missing": func(w *Window) { w.BenchmarkPassed = false },
		"benchmark stale":   func(w *Window) { w.BenchmarkAt = now.Add(-benchmarkMaxAge - time.Nanosecond) },
		"fewer than ten outcomes": func(w *Window) {
			w.FinishedAttempts = 9
			w.SuccessfulAttempts = 9
			w.LatencySamples = 9
		},
		"no latency sample": func(w *Window) { w.LatencySamples = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			window := base
			mutate(&window)
			result, err := Compute(window)
			if err != nil {
				t.Fatal(err)
			}
			if result.Rank != provisionalRank || !result.Provisional {
				t.Fatalf("rank result = %+v, want provisional rank %d", result, provisionalRank)
			}
		})
	}
}

func TestComputeQuarantineAlwaysRemovesNodeFromRouting(t *testing.T) {
	window := readyWindow(time.Now().UTC())
	window.Quarantined = true
	result, err := Compute(window)
	if err != nil {
		t.Fatal(err)
	}
	if result.Rank != 0 || result.Provisional || result.FormulaVersion != FormulaVersion {
		t.Fatalf("quarantined rank result = %+v", result)
	}
}

func TestComputeCapsMetricsAndUsesSuccessPrior(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	window := readyWindow(now)
	window.TestedCapacityBytes = 48 * 1024 * 1024 * 1024
	window.VerifiedOutputTokens = 1_000_000
	window.BenchmarkElapsedMS = 1_000
	window.ConnectedHeartbeatIntervals = 200
	window.P50ServerLatencyMillis = 3_000
	window.SuccessfulAttempts = 9
	window.FinishedAttempts = 10
	result, err := Compute(window)
	if err != nil {
		t.Fatal(err)
	}
	// 20 + 30 + 20 + 0 + 20*(10/12) = 86.666..., floored once.
	if result.Rank != 86 || result.Provisional {
		t.Fatalf("rank result = %+v, want 86", result)
	}
}

func TestComputeRejectsInvalidObservedWindow(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	tests := map[string]func(*Window){
		"success exceeds finished": func(w *Window) { w.SuccessfulAttempts = w.FinishedAttempts + 1 },
		"latency exceeds finished": func(w *Window) { w.LatencySamples = w.FinishedAttempts + 1 },
		"zero expected intervals":  func(w *Window) { w.ExpectedHeartbeatIntervals = 0 },
		"zero tested capacity":     func(w *Window) { w.TestedCapacityBytes = 0 },
		"zero benchmark duration":  func(w *Window) { w.BenchmarkElapsedMS = 0 },
		"future benchmark":         func(w *Window) { w.BenchmarkAt = now.Add(time.Second) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			window := readyWindow(now)
			mutate(&window)
			_, err := Compute(window)
			if !errors.Is(err, ErrInvalidWindow) {
				t.Fatalf("error = %v, want %v", err, ErrInvalidWindow)
			}
		})
	}
}
