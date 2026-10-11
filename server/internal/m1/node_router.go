package m1

import (
	"context"
	"errors"
	"math/rand"
	"sort"
	"sync"
	"time"

	"cordova/server/internal/request"

	"github.com/redis/go-redis/v9"
)

const (
	maxC3RoutingCandidates    = 100
	maxConcurrentModelQueries = 8
	nodeModelQueryTimeout     = 2 * time.Second
)

type c3RequestRouter struct {
	nodes      *nodeService
	controller *nodeModelController
	benchmarks *nodeBenchmarkScheduler
	modelLoads *nodeModelLoadScheduler
	catalog    NodeCatalogSnapshot
	models     map[string]NodeCatalogManifest
}

type c3RouteCandidate struct {
	nodeID       string
	profile      nodeCapacityProfile
	activeLeases int64
	rank         int
	sessionID    string
	generation   uint64
	hardware     nodeHardware
}

func NewC3RequestRouter(store *Store, tokenizers ...request.Tokenizer) (request.Router, error) {
	if store == nil {
		return nil, errors.New("M1 store is required for C3 routing")
	}
	if len(tokenizers) > 1 {
		return nil, errors.New("C3 routing accepts at most one trusted tokenizer")
	}
	nodes, err := store.ensureNodeService(store.cfg)
	if err != nil {
		return nil, err
	}
	catalog, err := ParseNodeCatalog(store.cfg.NodeCatalogSnapshot, store.cfg.NodeCatalogPublicKey)
	if err != nil {
		return nil, err
	}
	if len(catalog.Models) == 0 {
		return nil, errors.New("C3 routing requires at least one signed catalog model")
	}
	controller, err := newNodeModelController(nodes)
	if err != nil {
		return nil, err
	}
	router := &c3RequestRouter{nodes: nodes, controller: controller, catalog: catalog,
		models: make(map[string]NodeCatalogManifest, len(catalog.Models))}
	for _, manifest := range catalog.Models {
		router.models[manifest.Model.ID] = manifest
	}
	if len(tokenizers) == 1 {
		router.benchmarks = newNodeBenchmarkScheduler(nodes, controller, catalog, router.models, tokenizers[0])
		router.modelLoads = newNodeModelLoadScheduler(nodes, controller, catalog, router.models, router.benchmarks)
	}
	return router, nil
}

var _ request.Router = (*c3RequestRouter)(nil)

func (r *c3RequestRouter) Close() error {
	if r != nil && r.modelLoads != nil {
		r.modelLoads.close()
	}
	if r != nil && r.benchmarks != nil {
		r.benchmarks.close()
	}
	return nil
}

func (r *c3RequestRouter) Acquire(ctx context.Context, modelID string, excluded map[string]struct{}, deadline time.Time) (request.Attempt, error) {
	if ctx == nil || r == nil || r.nodes == nil {
		return nil, request.ErrNoCapacity
	}
	manifest, exists := r.models[modelID]
	if !exists {
		return nil, request.ErrModelNotFound
	}
	now := time.Now().UTC()
	if !deadline.After(now) {
		return nil, request.ErrNoCapacity
	}
	routeCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	ctx = routeCtx
	requiredCapacity, ok := oneSlotCapacity(manifest)
	if !ok {
		return nil, request.ErrNoCapacity
	}
	profiles, err := r.freshProfiles(ctx, manifest, requiredCapacity, excluded, now)
	if err != nil {
		return nil, err
	}
	if len(profiles) == 0 {
		warming, err := r.scheduleModelPreparation(ctx, manifest, excluded, now)
		if err != nil {
			return nil, err
		}
		if warming {
			return nil, request.ErrModelWarming
		}
		return nil, request.ErrNoCapacity
	}
	states, err := r.queryModelStates(ctx, manifest, profiles)
	if err != nil {
		return nil, err
	}
	var loaded, onDisk []c3RouteCandidate
	for _, profile := range profiles {
		state, ok := states[profile.nodeID]
		if !ok || state.ReasonCode != nil || (state.State != "loaded" && state.State != "on_disk") ||
			state.ArtifactSHA256 == nil || *state.ArtifactSHA256 != manifest.ArtifactSHA256 {
			continue
		}
		active, err := r.nodes.activeNodeModelLeaseCount(ctx, profile.nodeID, profile.sessionID,
			profile.generation, modelID)
		if err != nil {
			return nil, err
		}
		candidate := profile
		candidate.activeLeases = active
		candidate.rank = profile.profile.Rank
		if state.State == "loaded" && active < int64(profile.profile.VerifiedSlots) {
			loaded = append(loaded, candidate)
		} else if state.State == "on_disk" && active == 0 {
			onDisk = append(onDisk, candidate)
		}
	}
	loadedIdle, loadedBusy := make([]c3RouteCandidate, 0), make([]c3RouteCandidate, 0)
	for _, candidate := range loaded {
		if candidate.activeLeases == 0 {
			loadedIdle = append(loadedIdle, candidate)
		} else {
			loadedBusy = append(loadedBusy, candidate)
		}
	}
	rankC3Candidates(loadedIdle)
	rankC3Candidates(loadedBusy)
	for _, candidates := range [][]c3RouteCandidate{loadedIdle, loadedBusy} {
		for _, candidate := range candidates {
			attempt, err := r.nodes.acquireNodeInferenceAttempt(ctx, candidate.nodeID, modelID, r.catalog.Version,
				manifest.ArtifactSHA256, requiredCapacity, deadline)
			if err == nil {
				return attempt, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !errors.Is(err, ErrNodeNoCapacity) && !errors.Is(err, ErrNodeSlotUnavailable) &&
				!errors.Is(err, ErrNodeSessionUnavailable) && !errors.Is(err, ErrNodeConflict) &&
				!errors.Is(err, ErrNodeBenchmarkActive) && !errors.Is(err, ErrNodePreparationActive) {
				return nil, err
			}
		}
	}
	rankC3Candidates(onDisk)
	preparationExcluded := make(map[string]struct{}, len(excluded)+len(onDisk))
	for nodeID := range excluded {
		preparationExcluded[nodeID] = struct{}{}
	}
	for _, candidate := range onDisk {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if r.modelLoads == nil {
			break
		}
		preparationExcluded[candidate.nodeID] = struct{}{}
		loadCtx, cancel := context.WithDeadline(ctx, deadline)
		err := r.modelLoads.loadOnDisk(loadCtx, candidate, manifest)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if !retryableNodePreparationError(err) {
				return nil, err
			}
			continue
		}
		attempt, err := r.nodes.acquireNodeInferenceAttempt(ctx, candidate.nodeID, modelID, r.catalog.Version,
			manifest.ArtifactSHA256, requiredCapacity, deadline)
		if err == nil {
			return attempt, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !retryableNodePreparationError(err) {
			return nil, err
		}
	}
	if !deadline.After(time.Now()) {
		return nil, request.ErrNoCapacity
	}
	warming, err := r.scheduleModelPreparation(ctx, manifest, preparationExcluded, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if warming {
		return nil, request.ErrModelWarming
	}
	return nil, request.ErrNoCapacity
}

func retryableNodePreparationError(err error) bool {
	return errors.Is(err, ErrNodeNoCapacity) || errors.Is(err, ErrNodeSlotUnavailable) ||
		errors.Is(err, ErrNodeSessionUnavailable) || errors.Is(err, ErrNodeConflict) ||
		errors.Is(err, ErrNodeBenchmarkActive) || errors.Is(err, ErrNodePreparationActive) ||
		errors.Is(err, ErrNodeModelCommandRejected) || errors.Is(err, context.DeadlineExceeded)
}

func (r *c3RequestRouter) freshProfiles(ctx context.Context, manifest NodeCatalogManifest, requiredCapacity uint64,
	excluded map[string]struct{}, now time.Time) ([]c3RouteCandidate, error) {
	peers, err := r.freshLivePeers(ctx, excluded, now)
	if err != nil {
		return nil, err
	}
	result := make([]c3RouteCandidate, 0, len(peers))
	for _, peer := range peers {
		profileBytes, err := r.nodes.redis.Get(ctx, nodeCapacityProfileKey(peer.nodeID, manifest.Model.ID)).Bytes()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			return nil, err
		}
		var profile nodeCapacityProfile
		if decodeStrictBytes(profileBytes, &profile) != nil || profile.NodeID != peer.nodeID ||
			profile.ModelID != manifest.Model.ID || profile.CatalogVersion != r.catalog.Version ||
			profile.ArtifactSHA256 != manifest.ArtifactSHA256 || !profile.BenchmarkPassed || profile.Rank == 0 ||
			profile.VerifiedSlots == 0 || profile.VerifiedSlots > 64 || profile.VerifiedCapacityBytes < requiredCapacity ||
			profile.VerifiedCapacityBytes > (1<<53)-1 || profile.BenchmarkedAtUnix <= 0 || profile.BenchmarkedAtUnix > now.Unix() ||
			profile.ExpiresAtUnix <= now.Unix() || profile.ExpiresAtUnix > profile.BenchmarkedAtUnix+int64(nodeCapacityProfileMaxAge.Seconds()) ||
			profile.Rank < 0 || profile.Rank > 100 || profile.FirstTokenLatencyMS == 0 || profile.FirstTokenLatencyMS > 30_000 ||
			profile.VerifiedTokensPerSecondMilli < 2_000 || profile.VerifiedTokensPerSecondMilli > (1<<53)-1 {
			continue
		}
		peer.profile = profile
		result = append(result, peer)
	}
	return result, nil
}

func (r *c3RequestRouter) freshLivePeers(ctx context.Context, excluded map[string]struct{}, now time.Time) ([]c3RouteCandidate, error) {
	r.nodes.peerMu.RLock()
	peers := make([]*nodePeer, 0, len(r.nodes.peers))
	for nodeID, peer := range r.nodes.peers {
		if _, skip := excluded[nodeID]; skip || peer == nil {
			continue
		}
		peers = append(peers, peer)
	}
	r.nodes.peerMu.RUnlock()
	if len(peers) > maxC3RoutingCandidates {
		rand.Shuffle(len(peers), func(i, j int) { peers[i], peers[j] = peers[j], peers[i] })
		peers = peers[:maxC3RoutingCandidates]
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].identity.NodeID < peers[j].identity.NodeID })
	result := make([]c3RouteCandidate, 0, len(peers))
	for _, peer := range peers {
		if peer.identity.Generation == 0 || peer.identity.SessionID != peer.sessionID {
			continue
		}
		heartbeatBytes, err := r.nodes.redis.Get(ctx, nodeHeartbeatKey(peer.identity.NodeID)).Bytes()
		if err == redis.Nil {
			continue
		}
		if err != nil {
			return nil, err
		}
		var heartbeat nodeSessionHeartbeat
		if decodeStrictBytes(heartbeatBytes, &heartbeat) != nil || heartbeat.NodeID != peer.identity.NodeID || heartbeat.SessionID != peer.sessionID {
			continue
		}
		received, err := time.Parse(time.RFC3339Nano, heartbeat.Received)
		if err != nil || received.After(now) || now.Sub(received) >= nodeHeartbeatLimit {
			continue
		}
		result = append(result, c3RouteCandidate{nodeID: peer.identity.NodeID,
			sessionID: peer.sessionID, generation: peer.identity.Generation, hardware: heartbeat.Payload.Hardware})
	}
	return result, nil
}

func (r *c3RequestRouter) scheduleModelPreparation(ctx context.Context, manifest NodeCatalogManifest,
	excluded map[string]struct{}, now time.Time) (bool, error) {
	if r.benchmarks == nil && r.modelLoads == nil {
		return false, nil
	}
	peers, err := r.freshLivePeers(ctx, excluded, now)
	if err != nil || len(peers) == 0 {
		return false, err
	}
	byNode := make(map[string]nodeCapacityProfile)
	if requiredCapacity, ok := oneSlotCapacity(manifest); ok {
		profiles, profileErr := r.freshProfiles(ctx, manifest, requiredCapacity, excluded, now)
		if profileErr != nil {
			return false, profileErr
		}
		byNode = make(map[string]nodeCapacityProfile, len(profiles))
		for _, profile := range profiles {
			byNode[profile.nodeID] = profile.profile
		}
		for index := range peers {
			if profile, exists := byNode[peers[index].nodeID]; exists {
				peers[index].profile = profile
				peers[index].rank = profile.Rank
			}
		}
		rankC3Candidates(peers)
	}
	states, err := r.queryModelStates(ctx, manifest, peers)
	if err != nil {
		return false, err
	}
	scheduled := false
	for _, peer := range peers {
		state, ok := states[peer.nodeID]
		if !ok || state.ReasonCode != nil {
			continue
		}
		switch state.State {
		case "loaded":
			if state.ArtifactSHA256 == nil || *state.ArtifactSHA256 != manifest.ArtifactSHA256 {
				continue
			}
			if _, currentProfile := byNode[peer.nodeID]; currentProfile {
				continue
			}
			busy, busyErr := r.nodes.hasActiveNodeLeases(ctx, peer.nodeID, peer.sessionID, peer.generation)
			if busyErr != nil {
				return false, busyErr
			}
			if busy {
				continue
			}
			if r.benchmarks != nil && r.benchmarks.schedule(peer.nodeID, manifest.Model.ID, peer.hardware) {
				scheduled = true
			}
		case "on_disk":
			if state.ArtifactSHA256 == nil || *state.ArtifactSHA256 != manifest.ArtifactSHA256 {
				continue
			}
			busy, busyErr := r.nodes.hasActiveNodeLeases(ctx, peer.nodeID, peer.sessionID, peer.generation)
			if busyErr != nil {
				return false, busyErr
			}
			if busy {
				continue
			}
			if r.modelLoads != nil && r.modelLoads.schedule(peer, manifest.Model.ID, state.State, 0) {
				scheduled = true
			}
		case "absent":
			if state.ArtifactSHA256 != nil || state.AvailableDiskBytes < manifest.ArtifactSizeBytes {
				continue
			}
			busy, busyErr := r.nodes.hasActiveNodeLeases(ctx, peer.nodeID, peer.sessionID, peer.generation)
			if busyErr != nil {
				return false, busyErr
			}
			if busy {
				continue
			}
			if r.modelLoads != nil && r.modelLoads.schedule(peer, manifest.Model.ID, state.State, manifest.ArtifactSizeBytes) {
				scheduled = true
			}
		}
	}
	if !scheduled {
		scheduled = r.benchmarks.activeForModel(manifest.Model.ID) || r.modelLoads.activeForModel(manifest.Model.ID)
	}
	return scheduled, nil
}

func (r *c3RequestRouter) queryModelStates(ctx context.Context, manifest NodeCatalogManifest,
	candidates []c3RouteCandidate) (map[string]nodeModelStatePayload, error) {
	states := make(map[string]nodeModelStatePayload, len(candidates))
	if len(candidates) == 0 {
		return states, nil
	}
	semaphore := make(chan struct{}, maxConcurrentModelQueries)
	results := make(chan nodeModelQueryResult, len(candidates))
	var wait sync.WaitGroup
	for _, candidate := range candidates {
		wait.Add(1)
		go func(candidate c3RouteCandidate) {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				results <- nodeModelQueryResult{nodeID: candidate.nodeID, err: ctx.Err()}
				return
			}
			defer func() { <-semaphore }()
			state, ok, stateErr := r.freshModelState(ctx, candidate.nodeID, manifest.Model.ID)
			if stateErr != nil {
				results <- nodeModelQueryResult{nodeID: candidate.nodeID, err: stateErr}
				return
			}
			if !ok {
				queryCtx, cancel := context.WithTimeout(ctx, nodeModelQueryTimeout)
				_, _ = r.controller.Query(queryCtx, candidate.nodeID, manifest.Model.ID, r.catalog.Version)
				cancel()
				state, ok, stateErr = r.freshModelState(ctx, candidate.nodeID, manifest.Model.ID)
			}
			if stateErr != nil {
				results <- nodeModelQueryResult{nodeID: candidate.nodeID, err: stateErr}
				return
			}
			results <- nodeModelQueryResult{nodeID: candidate.nodeID, state: state, ok: ok}
		}(candidate)
	}
	wait.Wait()
	close(results)
	for result := range results {
		if result.err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, result.err
		}
		if result.ok {
			states[result.nodeID] = result.state
		}
	}
	return states, nil
}

type nodeModelQueryResult struct {
	nodeID string
	state  nodeModelStatePayload
	ok     bool
	err    error
}

func (r *c3RequestRouter) freshModelState(ctx context.Context, nodeID, modelID string) (nodeModelStatePayload, bool, error) {
	data, err := r.nodes.redis.Get(ctx, nodeModelStateKey(nodeID, modelID)).Bytes()
	if err == redis.Nil {
		return nodeModelStatePayload{}, false, nil
	}
	if err != nil {
		return nodeModelStatePayload{}, false, err
	}
	var record struct {
		SessionID string                `json:"session_id"`
		NodeID    string                `json:"node_id"`
		Received  string                `json:"received_at"`
		Model     nodeModelStatePayload `json:"model"`
	}
	if decodeStrictBytes(data, &record) != nil || record.NodeID != nodeID || !validID(record.SessionID) || record.Model.ModelID != modelID {
		return nodeModelStatePayload{}, false, nil
	}
	peer, ok := r.nodes.peerSnapshot(nodeID)
	if !ok || peer.sessionID != record.SessionID {
		return nodeModelStatePayload{}, false, nil
	}
	received, err := time.Parse(time.RFC3339Nano, record.Received)
	now := time.Now().UTC()
	if err != nil || received.After(now) || now.Sub(received) >= nodeHeartbeatLimit {
		return nodeModelStatePayload{}, false, nil
	}
	return record.Model, true, nil
}

func oneSlotCapacity(manifest NodeCatalogManifest) (uint64, bool) {
	if manifest.MemoryPerSlotBytes > (1<<53)-1 || manifest.MinMemoryBytes > (1<<53)-1-manifest.MemoryPerSlotBytes {
		return 0, false
	}
	return manifest.MinMemoryBytes + manifest.MemoryPerSlotBytes, true
}

func rankC3Candidates(candidates []c3RouteCandidate) {
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].rank != candidates[j].rank {
			return candidates[i].rank > candidates[j].rank
		}
		if candidates[i].profile.FirstTokenLatencyMS != candidates[j].profile.FirstTokenLatencyMS {
			return candidates[i].profile.FirstTokenLatencyMS < candidates[j].profile.FirstTokenLatencyMS
		}
		return candidates[i].nodeID < candidates[j].nodeID
	})
	for start := 0; start < len(candidates); {
		end := start + 1
		for end < len(candidates) && candidates[end].rank == candidates[start].rank &&
			candidates[end].profile.FirstTokenLatencyMS == candidates[start].profile.FirstTokenLatencyMS {
			end++
		}
		if end-start > 1 {
			rand.Shuffle(end-start, func(i, j int) { candidates[start+i], candidates[start+j] = candidates[start+j], candidates[start+i] })
		}
		start = end
	}
}
