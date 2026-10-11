package m1

import (
	"context"
	"errors"
	"time"

	"cordova/server/internal/inference"
	"cordova/server/internal/request"
)

const nodeAttemptCleanupTimeout = 2 * time.Second

type nodeInferenceAttempt struct {
	nodes          *nodeService
	nodeID         string
	modelID        string
	catalogVersion string
	attemptID      string
	leaseID        string
	deadline       time.Time
	releaseLease   func(context.Context)
	claimedInput   *uint32
	claimedOutput  *uint32
}

func newNodeInferenceAttempt(nodes *nodeService, nodeID, modelID, catalogVersion, attemptID, leaseID string,
	releaseLease func(context.Context)) (*nodeInferenceAttempt, error) {
	if nodes == nil || !validID(nodeID) || !validModelSlug(modelID) || !validID(catalogVersion) ||
		!validID(attemptID) || !validID(leaseID) {
		return nil, request.ErrInvalidRequest
	}
	return &nodeInferenceAttempt{nodes: nodes, nodeID: nodeID, modelID: modelID, catalogVersion: catalogVersion,
		attemptID: attemptID, leaseID: leaseID, releaseLease: releaseLease}, nil
}

var _ request.Attempt = (*nodeInferenceAttempt)(nil)
var _ request.AcceptedAttempt = (*nodeInferenceAttempt)(nil)
var _ request.OutcomeReporter = (*nodeInferenceAttempt)(nil)
var _ request.OutputClaimAuditor = (*nodeInferenceAttempt)(nil)

func (a *nodeInferenceAttempt) OwnerID() string { return a.nodeID }

func (a *nodeInferenceAttempt) Run(ctx context.Context, input inference.Request, emit func(string) error) (string, error) {
	return a.RunAccepted(ctx, input, nil, emit)
}

func (a *nodeInferenceAttempt) RunAccepted(ctx context.Context, input inference.Request, accepted func() error,
	emit func(string) error) (string, error) {
	if ctx == nil || emit == nil || a == nil || a.nodes == nil || input.CatalogVersion != a.catalogVersion ||
		input.MaxTokens < 1 || input.MaxTokens > 2048 ||
		len(input.InputTokenIDs) == 0 || len(input.InputTokenIDs) > 8192 || input.InputTokens != int64(len(input.InputTokenIDs)) {
		return "", request.ErrInvalidRequest
	}
	seed, err := nodeSeed(input.Parameters.Seed)
	if err != nil {
		return "", request.ErrInvalidRequest
	}
	for _, tokenID := range input.InputTokenIDs {
		if tokenID > 2_147_483_647 {
			return "", request.ErrInvalidRequest
		}
	}
	deadline := time.Now().UTC().Add(maxNodeLeaseLifetime)
	if !a.deadline.IsZero() && a.deadline.Before(deadline) {
		deadline = a.deadline
	}
	if supplied, ok := ctx.Deadline(); ok {
		if supplied.Before(deadline) {
			deadline = supplied.UTC()
		}
	}
	if !deadline.After(time.Now()) {
		return "", context.DeadlineExceeded
	}
	payload := nodeInferenceStartPayload{
		AttemptID: a.attemptID, LeaseID: a.leaseID, ModelID: a.modelID, CatalogVersion: a.catalogVersion,
		InputTokenIDs: input.InputTokenIDs, InputTokens: uint32(input.InputTokens), MaxTokens: uint32(input.MaxTokens),
		Parameters: nodeInferenceParameters{Temperature: input.Parameters.Temperature, TopP: input.Parameters.TopP,
			Stop: append([]string{}, input.Parameters.Stop...), Seed: seed},
		DeadlineAt: deadline.Format(time.RFC3339Nano), Kind: "user",
	}
	exchange, err := a.nodes.beginNodeExchange(ctx, a.nodeID, "inference.start", payload)
	if err != nil {
		return "", request.ErrNodeFailed
	}
	defer exchange.Close()

	acceptedSeen := false
	for {
		frame, nextErr := exchange.Next(ctx)
		if nextErr != nil {
			if ctx.Err() != nil {
				a.cancelNodeInference(ctx.Err())
				return "", ctx.Err()
			}
			return "", request.ErrNodeFailed
		}
		switch frame.Type {
		case "inference.accepted":
			var ack struct {
				AttemptID string `json:"attempt_id"`
				LeaseID   string `json:"lease_id"`
			}
			if acceptedSeen || decodeNodePayload(frame.Payload, &ack, "attempt_id", "lease_id") != nil ||
				ack.AttemptID != a.attemptID || ack.LeaseID != a.leaseID {
				return "", request.ErrNodeFailed
			}
			acceptedSeen = true
			if accepted != nil {
				if err := accepted(); err != nil {
					a.cancelNodeInference(err)
					return "", err
				}
			}
		case "inference.delta":
			var delta struct {
				AttemptID  string `json:"attempt_id"`
				LeaseID    string `json:"lease_id"`
				ChunkIndex uint32 `json:"chunk_index"`
				Text       string `json:"text"`
			}
			if !acceptedSeen || decodeNodePayload(frame.Payload, &delta, "attempt_id", "lease_id", "chunk_index", "text") != nil ||
				delta.AttemptID != a.attemptID || delta.LeaseID != a.leaseID {
				return "", request.ErrNodeFailed
			}
			if err := emit(delta.Text); err != nil {
				a.cancelNodeInference(err)
				return "", err
			}
		case "inference.complete":
			var complete struct {
				AttemptID    string  `json:"attempt_id"`
				LeaseID      string  `json:"lease_id"`
				LastChunk    *uint32 `json:"last_chunk_index"`
				FinishReason string  `json:"finish_reason"`
				InputTokens  uint32  `json:"claimed_input_tokens"`
				OutputTokens uint32  `json:"claimed_output_tokens"`
			}
			if !acceptedSeen || decodeNodePayload(frame.Payload, &complete, "attempt_id", "lease_id", "last_chunk_index", "finish_reason", "claimed_input_tokens", "claimed_output_tokens") != nil {
				return "", request.ErrNodeFailed
			}
			claimedInput, claimedOutput := complete.InputTokens, complete.OutputTokens
			a.claimedInput, a.claimedOutput = &claimedInput, &claimedOutput
			if complete.AttemptID != a.attemptID || complete.LeaseID != a.leaseID ||
				complete.InputTokens != uint32(input.InputTokens) || complete.OutputTokens > uint32(input.MaxTokens) ||
				complete.FinishReason != "stop" && complete.FinishReason != "length" {
				return "", request.ErrNodeFailed
			}
			return complete.FinishReason, nil
		case "error":
			return "", request.ErrNodeFailed
		default:
			return "", request.ErrNodeFailed
		}
	}
}

func (a *nodeInferenceAttempt) OutputClaims() (*uint32, *uint32) {
	if a == nil {
		return nil, nil
	}
	return a.claimedInput, a.claimedOutput
}

func (a *nodeInferenceAttempt) RecordOutcome(ctx context.Context, report request.OutcomeReport) error {
	if a == nil || a.nodes == nil || !validID(report.RequestID) || !validID(report.AttemptID) ||
		report.ModelID != a.modelID || (report.Outcome != "success" && report.Outcome != "failure" && report.Outcome != "cancelled") ||
		report.ElapsedMS == 0 || report.ElapsedMS > 120_000 || report.VerifiedOutputTokens > 1_000_000 ||
		(report.FirstTokenMS != nil && *report.FirstTokenMS > report.ElapsedMS) {
		return request.ErrInvalidRequest
	}
	payload := map[string]any{
		"model_id": report.ModelID, "outcome": report.Outcome, "elapsed_ms": report.ElapsedMS,
		"first_token_ms": report.FirstTokenMS, "verified_output_tokens": report.VerifiedOutputTokens,
		"claim_deviation": report.ClaimDeviation,
	}
	return a.nodes.appendNodeEvent(ctx, a.nodeID, "node.request_outcome", &report.RequestID, &report.AttemptID, payload)
}

func (a *nodeInferenceAttempt) Release(ctx context.Context) {
	if a != nil && a.releaseLease != nil {
		a.releaseLease(ctx)
	}
}

func (a *nodeInferenceAttempt) cancelNodeInference(cause error) {
	if a == nil || a.nodes == nil {
		return
	}
	reason := "client_cancelled"
	if errors.Is(cause, context.DeadlineExceeded) {
		reason = "deadline"
	} else if errors.Is(cause, request.ErrOutputLimit) {
		reason = "output_limit"
	} else if errors.Is(cause, request.ErrNodeFailed) {
		reason = "server_shutdown"
	}
	ctx, cancel := context.WithTimeout(context.Background(), nodeAttemptCleanupTimeout)
	defer cancel()
	exchange, err := a.nodes.beginNodeExchange(ctx, a.nodeID, "inference.cancel", map[string]any{
		"attempt_id": a.attemptID, "lease_id": a.leaseID, "reason": reason,
	})
	if err != nil {
		return
	}
	defer exchange.Close()
	for {
		frame, err := exchange.Next(ctx)
		if err != nil || frame.Type == "inference.cancelled" || frame.Type == "error" {
			return
		}
	}
}

func nodeSeed(seed *int64) (*uint32, error) {
	if seed == nil {
		return nil, nil
	}
	if *seed < 0 || *seed > (1<<31)-1 {
		return nil, errors.New("node inference seed is outside the C3 range")
	}
	value := uint32(*seed)
	return &value, nil
}

type nodeInferenceStartPayload struct {
	AttemptID      string                  `json:"attempt_id"`
	LeaseID        string                  `json:"lease_id"`
	ModelID        string                  `json:"model_id"`
	CatalogVersion string                  `json:"catalog_version"`
	InputTokenIDs  []uint32                `json:"input_token_ids"`
	InputTokens    uint32                  `json:"input_tokens"`
	MaxTokens      uint32                  `json:"max_tokens"`
	Parameters     nodeInferenceParameters `json:"parameters"`
	DeadlineAt     string                  `json:"deadline_at"`
	Kind           string                  `json:"kind"`
}

type nodeInferenceParameters struct {
	Temperature float64  `json:"temperature"`
	TopP        float64  `json:"top_p"`
	Stop        []string `json:"stop"`
	Seed        *uint32  `json:"seed"`
}
