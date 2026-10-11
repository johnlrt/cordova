package m1

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"cordova/server/internal/domain"
	"cordova/server/internal/inference"
	"cordova/server/internal/request"
)

const (
	maxActiveNodeBenchmarks      = 1
	maxConcurrentBenchmarkCounts = 2
	maxBenchmarkOutputBytes      = 64 << 10
	maxBenchmarkTotalBytes       = 4 << 20
	nodeBenchmarkTimeout         = 120 * time.Second
	benchmarkJobTimeout          = 15 * time.Minute
)

type c3BenchmarkTokenizer interface {
	request.Tokenizer
	request.InputPreparer
}

type nodeBenchmarkScheduler struct {
	nodes      *nodeService
	controller *nodeModelController
	catalog    NodeCatalogSnapshot
	models     map[string]NodeCatalogManifest
	tokenizer  c3BenchmarkTokenizer
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	jobs       map[string]struct{}
	workers    sync.WaitGroup
}

type nodeBenchmarkObservation struct {
	text      strings.Builder
	firstText time.Time
}

type nodeBenchmarkTrial struct {
	benchmarkID          string
	firstToken           uint64
	tokensPerSecondMilli uint64
}

func newNodeBenchmarkScheduler(nodes *nodeService, controller *nodeModelController, catalog NodeCatalogSnapshot,
	models map[string]NodeCatalogManifest, tokenizer request.Tokenizer) *nodeBenchmarkScheduler {
	prepared, ok := tokenizer.(c3BenchmarkTokenizer)
	if !ok || nodes == nil || controller == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &nodeBenchmarkScheduler{nodes: nodes, controller: controller, catalog: catalog, models: models,
		tokenizer: prepared, ctx: ctx, cancel: cancel, jobs: make(map[string]struct{})}
}

func (s *nodeBenchmarkScheduler) schedule(nodeID, modelID string, hardware nodeHardware) bool {
	if s == nil || !validID(nodeID) || !validModelSlug(modelID) {
		return false
	}
	manifest, ok := s.models[modelID]
	if !ok {
		return false
	}
	if _, _, ok := benchmarkMemoryBudget(manifest, hardware); !ok {
		return false
	}
	key := nodeID + ":" + modelID
	s.mu.Lock()
	if _, exists := s.jobs[key]; exists {
		s.mu.Unlock()
		return true
	}
	if len(s.jobs) >= maxActiveNodeBenchmarks || s.ctx.Err() != nil {
		s.mu.Unlock()
		return false
	}
	s.jobs[key] = struct{}{}
	s.workers.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.workers.Done()
		defer func() {
			s.mu.Lock()
			delete(s.jobs, key)
			s.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(s.ctx, benchmarkJobTimeout)
		defer cancel()
		_ = s.calibrate(ctx, nodeID, manifest, hardware)
	}()
	return true
}

func (s *nodeBenchmarkScheduler) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cancel()
	s.mu.Unlock()
	s.workers.Wait()
}

func (s *nodeBenchmarkScheduler) activeForModel(modelID string) bool {
	if s == nil || !validModelSlug(modelID) {
		return false
	}
	suffix := ":" + modelID
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.jobs {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
}

func (s *nodeBenchmarkScheduler) calibrate(ctx context.Context, nodeID string, manifest NodeCatalogManifest, hardware nodeHardware) error {
	budget, maxSlots, ok := benchmarkMemoryBudget(manifest, hardware)
	if !ok {
		return ErrNodeNoCapacity
	}
	stateCtx, stateCancel := context.WithTimeout(ctx, nodeModelQueryTimeout)
	_, queryErr := s.controller.Query(stateCtx, nodeID, manifest.Model.ID, s.catalog.Version)
	stateCancel()
	if queryErr != nil {
		return queryErr
	}
	state, fresh, err := (&c3RequestRouter{nodes: s.nodes}).freshModelState(ctx, nodeID, manifest.Model.ID)
	if err != nil {
		return err
	}
	if !fresh || state.State != "loaded" || state.ArtifactSHA256 == nil || *state.ArtifactSHA256 != manifest.ArtifactSHA256 {
		return ErrNodeSlotUnavailable
	}
	lockID, err := domain.NewID()
	if err != nil {
		return err
	}
	sessionID, generation, err := s.nodes.acquireNodeBenchmarkLock(ctx, nodeID, manifest.Model.ID,
		s.catalog.Version, manifest.ArtifactSHA256, lockID.String())
	if err != nil {
		return err
	}
	releaseLock := true
	defer func() {
		if releaseLock {
			s.nodes.releaseNodeBenchmarkLock(context.Background(), nodeID, sessionID, manifest.Model.ID, lockID.String())
		}
	}()

	var best *nodeCapacityProfile
	failedBaseline := false
	for concurrency := uint32(1); concurrency <= maxSlots; concurrency++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		footprint, ok := benchmarkFootprint(manifest, concurrency)
		if !ok || footprint > budget {
			break
		}
		if err := s.nodes.renewNodeBenchmarkLock(ctx, nodeID, sessionID, manifest.Model.ID, lockID.String()); err != nil {
			return err
		}
		trial, terminal, err := s.runTrial(ctx, nodeID, manifest, lockID.String(), concurrency)
		if err != nil {
			if !terminal {
				releaseLock = false
				return err
			}
			if !errors.Is(err, ErrNodeBenchmarkRejected) {
				return err
			}
			if concurrency == 1 && !failedBaseline {
				failedBaseline = true
				continue
			}
			break
		}
		if trial.firstToken > 30_000 || trial.tokensPerSecondMilli < 2_000 {
			if concurrency == 1 && !failedBaseline {
				failedBaseline = true
				continue
			}
			break
		}
		now := time.Now().UTC()
		best = &nodeCapacityProfile{NodeID: nodeID, ModelID: manifest.Model.ID,
			CatalogVersion: s.catalog.Version, ArtifactSHA256: manifest.ArtifactSHA256,
			BenchmarkID: trial.benchmarkID, BenchmarkedAtUnix: now.Unix(),
			ExpiresAtUnix: now.Add(nodeCapacityProfileMaxAge).Unix(), BenchmarkPassed: true,
			VerifiedSlots: concurrency, VerifiedCapacityBytes: footprint, Rank: 10,
			FirstTokenLatencyMS:          trial.firstToken,
			VerifiedTokensPerSecondMilli: trial.tokensPerSecondMilli}
	}
	if best == nil {
		return ErrNodeBenchmarkRejected
	}
	if err := s.nodes.renewNodeBenchmarkLock(ctx, nodeID, sessionID, manifest.Model.ID, lockID.String()); err != nil {
		return err
	}
	if err := s.nodes.setVerifiedNodeCapacity(ctx, *best, sessionID, generation, lockID.String()); err != nil {
		return err
	}
	return nil
}

func (s *nodeBenchmarkScheduler) runTrial(ctx context.Context, nodeID string, manifest NodeCatalogManifest,
	lockID string, concurrency uint32) (nodeBenchmarkTrial, bool, error) {
	benchmarkID, err := domain.NewID()
	if err != nil {
		return nodeBenchmarkTrial{}, true, err
	}
	prepared, err := s.tokenizer.PrepareInput(ctx, manifest.Model.ID, []inference.Message{{Role: "user",
		Content: "Cordova capacity benchmark v1. Continue this synthetic challenge with a concise explanation. Challenge nonce: " + benchmarkID.String()}})
	if err != nil || prepared.CatalogVersion != s.catalog.Version || prepared.TokenCount < 1 ||
		prepared.TokenCount != int64(len(prepared.TokenIDs)) || prepared.ContextTokens < 256 ||
		uint64(prepared.TokenCount)+256 > uint64(prepared.ContextTokens) || !validBenchmarkTokenIDs(prepared.TokenIDs) {
		if err != nil {
			return nodeBenchmarkTrial{}, true, err
		}
		return nodeBenchmarkTrial{}, true, request.ErrTokenizerUnavailable
	}
	seedBytes := make([]byte, 4)
	if _, err := cryptorand.Read(seedBytes); err != nil {
		return nodeBenchmarkTrial{}, true, err
	}
	seed := binary.LittleEndian.Uint32(seedBytes) & 2_147_483_647
	deadline := time.Now().UTC().Add(nodeBenchmarkTimeout - 5*time.Second)
	minimumCapacity, ok := benchmarkFootprint(manifest, 1)
	if !ok {
		return nodeBenchmarkTrial{}, true, ErrNodeNoCapacity
	}
	leases, err := s.nodes.claimNodeBenchmarkSlots(ctx, nodeID, manifest.Model.ID, s.catalog.Version,
		manifest.ArtifactSHA256, lockID, benchmarkID.String(),
		minimumCapacity, concurrency, deadline)
	if err != nil {
		return nodeBenchmarkTrial{}, true, err
	}
	leasePayloads := make([]nodeBenchmarkLease, len(leases))
	for i, lease := range leases {
		leasePayloads[i] = nodeBenchmarkLease{SlotIndex: uint32(i), LeaseID: lease.leaseID}
	}
	challenge := nodeBenchmarkStartPayload{BenchmarkID: benchmarkID.String(), ModelID: manifest.Model.ID,
		CatalogVersion: s.catalog.Version, InputTokenIDs: prepared.TokenIDs, MaxTokens: 256,
		Seed: seed, RequiredConcurrency: concurrency, Leases: leasePayloads,
		DeadlineAt: deadline.Format(time.RFC3339Nano)}
	operationCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	started := time.Now()
	observations := make([]nodeBenchmarkObservation, concurrency)
	leaseBySlot := make([]string, len(leases))
	for i, lease := range leases {
		leaseBySlot[i] = lease.leaseID
	}
	var totalBytes int
	localInvalid := false
	result, err := s.nodes.runNodeBenchmark(operationCtx, nodeID, challenge, func(delta nodeBenchmarkDeltaPayload) error {
		if delta.BenchmarkID != benchmarkID.String() || delta.SlotIndex >= uint32(len(observations)) ||
			delta.LeaseID != leaseBySlot[delta.SlotIndex] || !utf8.ValidString(delta.Text) {
			localInvalid = true
			return nil
		}
		observation := &observations[delta.SlotIndex]
		if delta.Text == "" {
			return nil
		}
		if observation.firstText.IsZero() {
			observation.firstText = time.Now()
		}
		if observation.text.Len()+len(delta.Text) > maxBenchmarkOutputBytes || totalBytes+len(delta.Text) > maxBenchmarkTotalBytes {
			localInvalid = true
			return nil
		}
		observation.text.WriteString(delta.Text)
		totalBytes += len(delta.Text)
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNodeBenchmarkRejected) {
			releaseBenchmarkLeases(leases)
			return nodeBenchmarkTrial{}, true, err
		}
		return nodeBenchmarkTrial{}, false, err
	}
	ended := time.Now()
	releaseBenchmarkLeases(leases)
	if localInvalid || result.BenchmarkID != benchmarkID.String() || result.CompletedSlots != concurrency ||
		len(result.Slots) != int(concurrency) {
		return nodeBenchmarkTrial{}, true, ErrNodeBenchmarkRejected
	}
	seen := make(map[uint32]bool, concurrency)
	for _, slot := range result.Slots {
		if slot.SlotIndex >= concurrency || seen[slot.SlotIndex] || slot.LeaseID != leaseBySlot[slot.SlotIndex] ||
			slot.ClaimedOutputTokens > 256 || (slot.FinishReason != "stop" && slot.FinishReason != "length") {
			return nodeBenchmarkTrial{}, true, ErrNodeBenchmarkRejected
		}
		seen[slot.SlotIndex] = true
	}

	type countResult struct {
		index  int
		tokens int64
		err    error
	}
	countResults := make(chan countResult, concurrency)
	semaphore := make(chan struct{}, maxConcurrentBenchmarkCounts)
	var wait sync.WaitGroup
	for index := range observations {
		if observations[index].text.Len() == 0 || observations[index].firstText.IsZero() {
			return nodeBenchmarkTrial{}, true, ErrNodeBenchmarkRejected
		}
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				countResults <- countResult{index: index, err: ctx.Err()}
				return
			}
			defer func() { <-semaphore }()
			tokens, err := s.tokenizer.CountOutput(ctx, manifest.Model.ID, observations[index].text.String())
			countResults <- countResult{index: index, tokens: tokens, err: err}
		}(index)
	}
	wait.Wait()
	close(countResults)
	counts := make([]int64, concurrency)
	for item := range countResults {
		if item.err != nil || item.tokens < 1 || item.tokens > 256 {
			if item.err != nil {
				return nodeBenchmarkTrial{}, true, item.err
			}
			return nodeBenchmarkTrial{}, true, ErrNodeBenchmarkRejected
		}
		counts[item.index] = item.tokens
	}
	totalTokens := uint64(0)
	firstLatencies := make([]uint64, 0, concurrency)
	for index, observation := range observations {
		totalTokens += uint64(counts[index])
		latency := observation.firstText.Sub(started).Milliseconds()
		if latency < 1 {
			latency = 1
		}
		firstLatencies = append(firstLatencies, uint64(latency))
	}
	sort.Slice(firstLatencies, func(i, j int) bool { return firstLatencies[i] < firstLatencies[j] })
	firstToken := firstLatencies[len(firstLatencies)/2]
	elapsedMS := ended.Sub(started).Milliseconds()
	if elapsedMS < 1 {
		elapsedMS = 1
	}
	tokensPerSecondMilli := totalTokens * 1_000_000 / uint64(elapsedMS)
	return nodeBenchmarkTrial{benchmarkID: benchmarkID.String(), firstToken: firstToken,
		tokensPerSecondMilli: tokensPerSecondMilli}, true, nil
}

func releaseBenchmarkLeases(leases []*nodeSlotLease) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, lease := range leases {
		if ctx.Err() != nil {
			return
		}
		lease.Release(ctx)
	}
}

func benchmarkMemoryBudget(manifest NodeCatalogManifest, hardware nodeHardware) (uint64, uint32, bool) {
	var reported uint64
	switch hardware.Backend {
	case "cuda", "vulkan":
		reported = hardware.ReportedFreeVRAMBytes
	case "metal", "cpu":
		reported = hardware.RAMBytes
	default:
		return 0, 0, false
	}
	if reported == 0 || reported > (1<<53)-1 || manifest.MinMemoryBytes > (1<<53)-1 || manifest.MemoryPerSlotBytes == 0 ||
		manifest.MemoryPerSlotBytes > (1<<53)-1 {
		return 0, 0, false
	}
	budget := reported/5*4 + (reported%5)*4/5
	if budget < manifest.MinMemoryBytes || budget-manifest.MinMemoryBytes < manifest.MemoryPerSlotBytes {
		return budget, 0, false
	}
	max := (budget - manifest.MinMemoryBytes) / manifest.MemoryPerSlotBytes
	if max > 64 {
		max = 64
	}
	return budget, uint32(max), max > 0
}

func benchmarkFootprint(manifest NodeCatalogManifest, slots uint32) (uint64, bool) {
	if slots == 0 || manifest.MemoryPerSlotBytes > (1<<53)-1 || manifest.MinMemoryBytes > (1<<53)-1 {
		return 0, false
	}
	extra := manifest.MemoryPerSlotBytes * uint64(slots)
	if extra > (1<<53)-1-manifest.MinMemoryBytes {
		return 0, false
	}
	return manifest.MinMemoryBytes + extra, true
}
