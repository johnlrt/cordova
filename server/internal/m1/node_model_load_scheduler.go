package m1

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"cordova/server/internal/domain"
	"cordova/server/internal/inference"
)

const (
	maxActiveNodeModelLoads   = 2
	maxAbsentNodePreparations = 2
	nodeModelPrepareTimeout   = 12 * time.Minute
	nodeModelLockRenewEvery   = 10 * time.Second
)

type nodeModelLoadScheduler struct {
	nodes      *nodeService
	controller *nodeModelController
	catalog    NodeCatalogSnapshot
	models     map[string]NodeCatalogManifest
	benchmarks *nodeBenchmarkScheduler
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	jobs       map[string]struct{}
	workers    sync.WaitGroup
}

func newNodeModelLoadScheduler(nodes *nodeService, controller *nodeModelController, catalog NodeCatalogSnapshot,
	models map[string]NodeCatalogManifest, benchmarks *nodeBenchmarkScheduler) *nodeModelLoadScheduler {
	if nodes == nil || controller == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &nodeModelLoadScheduler{nodes: nodes, controller: controller, catalog: catalog, models: models,
		benchmarks: benchmarks, ctx: ctx, cancel: cancel, jobs: make(map[string]struct{})}
}

func (s *nodeModelLoadScheduler) schedule(candidate c3RouteCandidate, modelID, expectedState string,
	requiredDiskBytes uint64) bool {
	if s == nil || !validID(candidate.nodeID) || !validID(candidate.sessionID) || candidate.generation == 0 ||
		!validModelSlug(modelID) || (expectedState != "on_disk" && expectedState != "absent") {
		return false
	}
	manifest, ok := s.models[modelID]
	if !ok || (expectedState == "on_disk" && requiredDiskBytes != 0) ||
		(expectedState == "absent" && (requiredDiskBytes == 0 || requiredDiskBytes != manifest.ArtifactSizeBytes)) {
		return false
	}
	if _, _, ok := benchmarkMemoryBudget(manifest, candidate.hardware); !ok {
		return false
	}
	key := candidate.nodeID + ":" + candidate.sessionID + ":" + modelID
	s.mu.Lock()
	if _, exists := s.jobs[key]; exists {
		s.mu.Unlock()
		return true
	}
	if len(s.jobs) >= maxActiveNodeModelLoads || s.ctx.Err() != nil {
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
		ctx, cancel := context.WithTimeout(s.ctx, nodeModelPrepareTimeout)
		defer cancel()
		_ = s.prepare(ctx, candidate, manifest, expectedState, requiredDiskBytes)
	}()
	return true
}

func (s *nodeModelLoadScheduler) prepare(ctx context.Context, candidate c3RouteCandidate,
	manifest NodeCatalogManifest, expectedState string, requiredDiskBytes uint64) error {
	stateCtx, cancel := context.WithTimeout(ctx, nodeModelQueryTimeout)
	remote, err := s.controller.queryForSession(stateCtx, candidate.nodeID, manifest.Model.ID,
		s.catalog.Version, candidate.sessionID)
	cancel()
	if err != nil {
		return err
	}
	if !nodeModelStateMatches(remote, expectedState, manifest.ArtifactSHA256, requiredDiskBytes) {
		return ErrNodeSlotUnavailable
	}
	lockID, err := domain.NewID()
	if err != nil {
		return err
	}
	permitHeld := false
	if expectedState == "absent" {
		if err := s.nodes.acquireNodeModelPreparationPermit(ctx, lockID.String()); err != nil {
			return err
		}
		permitHeld = true
	}
	defer func() {
		if permitHeld {
			_ = s.nodes.releaseNodeModelPreparationPermit(context.Background(), lockID.String())
		}
	}()
	sessionID, generation, err := s.nodes.acquireNodeModelLoadLock(ctx, candidate.nodeID, manifest.Model.ID,
		s.catalog.Version, manifest.ArtifactSHA256, expectedState, requiredDiskBytes, lockID.String())
	if err != nil {
		return err
	}
	lockHeld := true
	defer func() {
		if lockHeld {
			_ = s.nodes.releaseNodeModelLoadLock(context.Background(), candidate.nodeID, sessionID, manifest.Model.ID, lockID.String())
		}
	}()
	if sessionID != candidate.sessionID || generation != candidate.generation {
		return ErrNodeConflict
	}

	leaseCtx, stopLease := context.WithCancel(ctx)
	renewed := make(chan error, 1)
	go s.renewLoadLock(leaseCtx, stopLease, candidate.nodeID, sessionID, manifest.Model.ID, lockID.String(), renewed)
	operation := "load"
	if expectedState == "absent" {
		operation = "download"
	}
	ready, err := s.controller.runModelCommandForSession(leaseCtx, candidate.nodeID, manifest.Model.ID,
		s.catalog.Version, "model."+operation, sessionID)
	if err == nil && (ready.ModelID != manifest.Model.ID || ready.ArtifactSHA256 != manifest.ArtifactSHA256 ||
		ready.TokenizerSHA256 != manifest.TokenizerSHA256 || ready.TemplateSHA256 != manifest.TemplateSHA256 || ready.MaxSlots == 0) {
		err = errors.New("prepared node model does not match the signed catalog")
	}
	if err == nil {
		stateCtx, cancel = context.WithTimeout(leaseCtx, nodeModelQueryTimeout)
		remote, err = s.controller.queryForSession(stateCtx, candidate.nodeID, manifest.Model.ID,
			s.catalog.Version, sessionID)
		cancel()
	}
	stopLease()
	lockErr := <-renewed
	if err == nil && lockErr != nil {
		err = lockErr
	}
	if err != nil {
		return err
	}
	if remote.State != "loaded" || remote.ArtifactSHA256 != manifest.ArtifactSHA256 {
		return ErrNodeSlotUnavailable
	}
	if err := s.nodes.releaseNodeModelLoadLock(context.Background(), candidate.nodeID, sessionID, manifest.Model.ID, lockID.String()); err != nil {
		return err
	}
	lockHeld = false
	if permitHeld {
		if err := s.nodes.releaseNodeModelPreparationPermit(context.Background(), lockID.String()); err != nil {
			return err
		}
		permitHeld = false
	}
	if s.benchmarks != nil {
		s.benchmarks.schedule(candidate.nodeID, manifest.Model.ID, candidate.hardware)
	}
	return nil
}

func (s *nodeModelLoadScheduler) loadOnDisk(ctx context.Context, candidate c3RouteCandidate,
	manifest NodeCatalogManifest) error {
	if s == nil || ctx == nil || !validID(candidate.nodeID) || !validID(candidate.sessionID) ||
		candidate.generation == 0 || !validModelSlug(manifest.Model.ID) {
		return ErrInvalid
	}
	stateCtx, cancel := context.WithTimeout(ctx, nodeModelQueryTimeout)
	remote, err := s.controller.queryForSession(stateCtx, candidate.nodeID, manifest.Model.ID,
		s.catalog.Version, candidate.sessionID)
	cancel()
	if err != nil {
		return err
	}
	if !nodeModelStateMatches(remote, "on_disk", manifest.ArtifactSHA256, 0) {
		return ErrNodeSlotUnavailable
	}
	lockID, err := domain.NewID()
	if err != nil {
		return err
	}
	sessionID, generation, err := s.nodes.acquireNodeModelLoadLock(ctx, candidate.nodeID, manifest.Model.ID,
		s.catalog.Version, manifest.ArtifactSHA256, "on_disk", 0, lockID.String())
	if err != nil {
		return err
	}
	lockHeld := true
	defer func() {
		if lockHeld {
			_ = s.nodes.releaseNodeModelLoadLock(context.Background(), candidate.nodeID, sessionID, manifest.Model.ID, lockID.String())
		}
	}()
	if sessionID != candidate.sessionID || generation != candidate.generation {
		return ErrNodeConflict
	}
	ready, err := s.controller.runModelCommandForSession(ctx, candidate.nodeID, manifest.Model.ID,
		s.catalog.Version, "model.load", sessionID)
	if err != nil {
		return err
	}
	if ready.ModelID != manifest.Model.ID || ready.ArtifactSHA256 != manifest.ArtifactSHA256 ||
		ready.TokenizerSHA256 != manifest.TokenizerSHA256 || ready.TemplateSHA256 != manifest.TemplateSHA256 || ready.MaxSlots == 0 {
		return errors.New("loaded node model does not match the signed catalog")
	}
	stateCtx, cancel = context.WithTimeout(ctx, nodeModelQueryTimeout)
	remote, err = s.controller.queryForSession(stateCtx, candidate.nodeID, manifest.Model.ID,
		s.catalog.Version, sessionID)
	cancel()
	if err != nil {
		return err
	}
	if remote.State != inference.ModelLoaded || remote.ArtifactSHA256 != manifest.ArtifactSHA256 {
		return ErrNodeSlotUnavailable
	}
	if err := s.nodes.releaseNodeModelLoadLock(context.Background(), candidate.nodeID, sessionID, manifest.Model.ID, lockID.String()); err != nil {
		return err
	}
	lockHeld = false
	return nil
}

func nodeModelStateMatches(remote inference.RemoteModelStatus, expectedState, artifactSHA256 string,
	requiredDiskBytes uint64) bool {
	if remote.ReasonCode != nil {
		return false
	}
	switch expectedState {
	case "on_disk":
		return remote.State == inference.ModelOnDisk && remote.ArtifactSHA256 == artifactSHA256 && requiredDiskBytes == 0
	case "absent":
		return remote.State == inference.ModelAbsent && remote.ArtifactSHA256 == "" &&
			requiredDiskBytes > 0 && remote.AvailableDiskBytes >= requiredDiskBytes
	default:
		return false
	}
}

func (s *nodeModelLoadScheduler) renewLoadLock(ctx context.Context, cancel context.CancelFunc,
	nodeID, sessionID, modelID, lockID string, result chan<- error) {
	ticker := time.NewTicker(nodeModelLockRenewEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			result <- nil
			return
		case <-ticker.C:
			renewCtx, stop := context.WithTimeout(ctx, 3*time.Second)
			err := s.nodes.renewNodeModelLoadLock(renewCtx, nodeID, sessionID, modelID, lockID)
			stop()
			if err != nil {
				result <- err
				cancel()
				return
			}
		}
	}
}

func (s *nodeModelLoadScheduler) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cancel()
	s.mu.Unlock()
	s.workers.Wait()
}

func (s *nodeModelLoadScheduler) activeForModel(modelID string) bool {
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
