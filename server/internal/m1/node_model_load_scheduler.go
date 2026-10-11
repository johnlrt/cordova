package m1

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"cordova/server/internal/domain"
)

const (
	maxActiveNodeModelLoads = 2
	nodeModelLoadTimeout    = 15 * time.Second
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

func (s *nodeModelLoadScheduler) schedule(candidate c3RouteCandidate, modelID string) bool {
	if s == nil || !validID(candidate.nodeID) || !validID(candidate.sessionID) || candidate.generation == 0 ||
		!validModelSlug(modelID) {
		return false
	}
	manifest, ok := s.models[modelID]
	if !ok {
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
		ctx, cancel := context.WithTimeout(s.ctx, nodeModelLoadTimeout)
		defer cancel()
		_ = s.load(ctx, candidate, manifest)
	}()
	return true
}

func (s *nodeModelLoadScheduler) load(ctx context.Context, candidate c3RouteCandidate, manifest NodeCatalogManifest) error {
	stateCtx, cancel := context.WithTimeout(ctx, nodeModelQueryTimeout)
	remote, err := s.controller.queryForSession(stateCtx, candidate.nodeID, manifest.Model.ID, s.catalog.Version, candidate.sessionID)
	cancel()
	if err != nil {
		return err
	}
	if remote.State != "on_disk" || remote.ArtifactSHA256 != manifest.ArtifactSHA256 {
		return ErrNodeSlotUnavailable
	}
	lockID, err := domain.NewID()
	if err != nil {
		return err
	}
	sessionID, generation, err := s.nodes.acquireNodeModelLoadLock(ctx, candidate.nodeID, manifest.Model.ID,
		s.catalog.Version, manifest.ArtifactSHA256, lockID.String())
	if err != nil {
		return err
	}
	defer s.nodes.releaseNodeModelLoadLock(context.Background(), candidate.nodeID, sessionID, manifest.Model.ID, lockID.String())
	if sessionID != candidate.sessionID || generation != candidate.generation {
		return ErrNodeConflict
	}
	ready, err := s.controller.loadVerifiedForSession(ctx, candidate.nodeID, manifest.Model.ID, s.catalog.Version, sessionID)
	if err != nil {
		return err
	}
	if ready.ModelID != manifest.Model.ID || ready.ArtifactSHA256 != manifest.ArtifactSHA256 ||
		ready.TokenizerSHA256 != manifest.TokenizerSHA256 || ready.TemplateSHA256 != manifest.TemplateSHA256 || ready.MaxSlots == 0 {
		return errors.New("loaded node model does not match the signed catalog")
	}
	stateCtx, cancel = context.WithTimeout(ctx, nodeModelQueryTimeout)
	remote, err = s.controller.queryForSession(stateCtx, candidate.nodeID, manifest.Model.ID, s.catalog.Version, sessionID)
	cancel()
	if err != nil {
		return err
	}
	if remote.State != "loaded" || remote.ArtifactSHA256 != manifest.ArtifactSHA256 {
		return ErrNodeSlotUnavailable
	}
	if s.benchmarks != nil {
		s.benchmarks.schedule(candidate.nodeID, manifest.Model.ID, candidate.hardware)
	}
	return nil
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
