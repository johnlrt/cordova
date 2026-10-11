package m1

import (
	"context"
	"errors"
)

var ErrNodeBenchmarkRejected = errors.New("node rejected or failed the benchmark challenge")

type nodeBenchmarkLease struct {
	SlotIndex uint32 `json:"slot_index"`
	LeaseID   string `json:"lease_id"`
}

type nodeBenchmarkStartPayload struct {
	BenchmarkID         string               `json:"benchmark_id"`
	ModelID             string               `json:"model_id"`
	CatalogVersion      string               `json:"catalog_version"`
	InputTokenIDs       []uint32             `json:"input_token_ids"`
	MaxTokens           uint32               `json:"max_tokens"`
	Seed                uint32               `json:"seed"`
	RequiredConcurrency uint32               `json:"required_concurrency"`
	Leases              []nodeBenchmarkLease `json:"leases"`
	DeadlineAt          string               `json:"deadline_at"`
}

type nodeBenchmarkDeltaPayload struct {
	BenchmarkID string `json:"benchmark_id"`
	SlotIndex   uint32 `json:"slot_index"`
	LeaseID     string `json:"lease_id"`
	ChunkIndex  uint32 `json:"chunk_index"`
	Text        string `json:"text"`
}

type nodeBenchmarkSlotResult struct {
	SlotIndex           uint32 `json:"slot_index"`
	LeaseID             string `json:"lease_id"`
	ClaimedOutputTokens uint32 `json:"claimed_output_tokens"`
	FinishReason        string `json:"finish_reason"`
}

type nodeBenchmarkResultPayload struct {
	BenchmarkID           string                    `json:"benchmark_id"`
	Slots                 []nodeBenchmarkSlotResult `json:"slots"`
	ReportedPeakVRAMBytes uint64                    `json:"reported_peak_vram_bytes"`
	CompletedSlots        uint32                    `json:"completed_slots"`
}

func validBenchmarkTokenIDs(tokens []uint32) bool {
	if len(tokens) == 0 || len(tokens) > 8192 {
		return false
	}
	for _, token := range tokens {
		if token > 2_147_483_647 {
			return false
		}
	}
	return true
}

// runNodeBenchmark keeps generated benchmark deltas in the caller's bounded,
// request-owned memory and returns only the node's per-slot summary.
func (s *nodeService) runNodeBenchmark(ctx context.Context, nodeID string, challenge nodeBenchmarkStartPayload,
	onDelta func(nodeBenchmarkDeltaPayload) error) (nodeBenchmarkResultPayload, error) {
	if s == nil || ctx == nil || !validID(nodeID) || !validID(challenge.BenchmarkID) || !validModelSlug(challenge.ModelID) ||
		!validID(challenge.CatalogVersion) || !validBenchmarkTokenIDs(challenge.InputTokenIDs) || challenge.MaxTokens != 256 ||
		challenge.Seed > 2_147_483_647 || challenge.RequiredConcurrency == 0 || challenge.RequiredConcurrency > 64 ||
		uint32(len(challenge.Leases)) != challenge.RequiredConcurrency {
		return nodeBenchmarkResultPayload{}, ErrInvalid
	}
	exchange, err := s.beginNodeExchange(ctx, nodeID, "benchmark.start", challenge)
	if err != nil {
		return nodeBenchmarkResultPayload{}, err
	}
	defer exchange.Close()
	var deltaErr error
	for {
		frame, err := exchange.Next(ctx)
		if err != nil {
			return nodeBenchmarkResultPayload{}, err
		}
		switch frame.Type {
		case "benchmark.delta":
			var delta nodeBenchmarkDeltaPayload
			if decodeNodePayload(frame.Payload, &delta, "benchmark_id", "slot_index", "lease_id", "chunk_index", "text") != nil {
				return nodeBenchmarkResultPayload{}, ErrNodeBenchmarkRejected
			}
			if onDelta != nil && deltaErr == nil {
				deltaErr = onDelta(delta)
			}
		case "benchmark.result":
			var result nodeBenchmarkResultPayload
			if decodeNodePayload(frame.Payload, &result, "benchmark_id", "slots", "reported_peak_vram_bytes", "completed_slots") != nil {
				return nodeBenchmarkResultPayload{}, ErrNodeBenchmarkRejected
			}
			if deltaErr != nil {
				return nodeBenchmarkResultPayload{}, deltaErr
			}
			return result, nil
		case "error":
			return nodeBenchmarkResultPayload{}, ErrNodeBenchmarkRejected
		default:
			return nodeBenchmarkResultPayload{}, ErrNodeBenchmarkRejected
		}
	}
}
