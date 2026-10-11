package m1

import (
	"context"
	"errors"
	"time"
)

type NodeModelPreparationStatus string

const (
	NodeModelPreparationReady        NodeModelPreparationStatus = "ready"
	NodeModelPreparationScheduled    NodeModelPreparationStatus = "preparing"
	NodeModelPreparationBenchmarking NodeModelPreparationStatus = "benchmarking"
)

var ErrNodeModelPreparationUnavailable = errors.New("node model preparation is unavailable")

// NodeModelPreparer schedules owner-requested preparation through the same
// session fencing, leases, and bounded workers used by inference routing.
type NodeModelPreparer interface {
	PrepareNodeModel(context.Context, string, string) (NodeModelPreparationStatus, error)
}

func (r *c3RequestRouter) PrepareNodeModel(ctx context.Context, nodeID, modelID string) (NodeModelPreparationStatus, error) {
	if ctx == nil || r == nil || r.nodes == nil || !validID(nodeID) || !validModelSlug(modelID) {
		return "", ErrInvalid
	}
	manifest, ok := r.models[modelID]
	if !ok {
		return "", ErrNotFound
	}
	if r.modelLoads == nil || r.benchmarks == nil {
		return "", ErrNodeModelPreparationUnavailable
	}
	now := time.Now().UTC()
	peers, err := r.freshLivePeers(ctx, nil, now)
	if err != nil {
		return "", err
	}
	var candidate *c3RouteCandidate
	for index := range peers {
		if peers[index].nodeID == nodeID {
			candidate = &peers[index]
			break
		}
	}
	if candidate == nil {
		return "", ErrNodeSessionUnavailable
	}
	if _, _, ok := benchmarkMemoryBudget(manifest, candidate.hardware); !ok {
		return "", ErrNodeNoCapacity
	}

	states, err := r.queryModelStates(ctx, manifest, []c3RouteCandidate{*candidate})
	if err != nil {
		return "", err
	}
	state, ok := states[nodeID]
	if !ok || state.ReasonCode != nil {
		return "", ErrNodeNoCapacity
	}
	if state.ArtifactSHA256 != nil && *state.ArtifactSHA256 != manifest.ArtifactSHA256 {
		return "", ErrNodeNoCapacity
	}

	if state.State == "loaded" {
		if state.ArtifactSHA256 == nil {
			return "", ErrNodeNoCapacity
		}
		requiredCapacity, ok := oneSlotCapacity(manifest)
		if !ok {
			return "", ErrNodeNoCapacity
		}
		profiles, err := r.freshProfiles(ctx, manifest, requiredCapacity, nil, now)
		if err != nil {
			return "", err
		}
		for _, profile := range profiles {
			if profile.nodeID == nodeID {
				return NodeModelPreparationReady, nil
			}
		}
		busy, err := r.nodes.hasActiveNodeLeases(ctx, nodeID, candidate.sessionID, candidate.generation)
		if err != nil {
			return "", err
		}
		if busy {
			return "", ErrNodeNoCapacity
		}
		if !r.benchmarks.schedule(nodeID, modelID, candidate.hardware) {
			return "", ErrNodePreparationActive
		}
		return NodeModelPreparationBenchmarking, nil
	}

	expectedState := state.State
	var requiredDiskBytes uint64
	switch expectedState {
	case "on_disk":
		if state.ArtifactSHA256 == nil {
			return "", ErrNodeNoCapacity
		}
	case "absent":
		if state.ArtifactSHA256 != nil || state.AvailableDiskBytes < manifest.ArtifactSizeBytes {
			return "", ErrNodeNoCapacity
		}
		requiredDiskBytes = manifest.ArtifactSizeBytes
	default:
		return "", ErrNodeNoCapacity
	}
	busy, err := r.nodes.hasActiveNodeLeases(ctx, nodeID, candidate.sessionID, candidate.generation)
	if err != nil {
		return "", err
	}
	if busy {
		return "", ErrNodeNoCapacity
	}
	if !r.modelLoads.schedule(*candidate, modelID, expectedState, requiredDiskBytes) {
		return "", ErrNodePreparationActive
	}
	return NodeModelPreparationScheduled, nil
}
