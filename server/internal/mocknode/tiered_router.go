package mocknode

import (
	"context"
	"encoding/hex"
	"errors"
	"math/rand"
	"sort"
	"sync"
	"time"

	"cordova/server/internal/api"
	"cordova/server/internal/domain"
	requestcoord "cordova/server/internal/request"
)

const (
	maxNodeHeartbeatAge  = 45 * time.Second
	maxBenchmarkAge      = 24 * time.Hour
	modelLoadDeadline    = 10 * time.Second
	modelWarmDeadline    = 10 * time.Minute
	maxConcurrentWarmups = 2
)

type ModelResidency string

const (
	ModelLoaded ModelResidency = "loaded"
	ModelOnDisk ModelResidency = "on_disk"
	ModelAbsent ModelResidency = "absent"
)

// NodeStatus is the server-observed catalog and benchmark projection used by
// the mock C6 router. The node's reported active count never grants capacity;
// this router counts only its own live leases.
type NodeStatus struct {
	NodeID                string
	SessionID             string
	Generation            uint64
	Online                bool
	LastHeartbeat         time.Time
	CatalogVersion        string
	ArtifactSHA256        string
	ModelState            ModelResidency
	BenchmarkedAt         time.Time
	VerifiedSlots         int
	VerifiedCapacityBytes int64
	AvailableDiskBytes    int64
	Rank                  int
	FirstTokenLatency     time.Duration
	Quarantined           bool
	Held                  bool
}

type TieredNode struct {
	Status   NodeStatus
	Provider *Provider
}

type RemoteModelStatus struct {
	State          ModelResidency
	ArtifactSHA256 string
}

// ModelController represents the C6 model.query/load/download exchanges.
// The production C3 transport adapter can implement the same operations.
type ModelController interface {
	Query(context.Context, string, string, string) (RemoteModelStatus, error)
	Load(context.Context, string, string, string) (artifactSHA256 string, err error)
	Download(context.Context, string, string, string) (artifactSHA256 string, err error)
}

type TieredRouterConfig struct {
	ModelID               string
	CatalogVersion        string
	ArtifactSHA256        string
	RequiredCapacityBytes int64
	RequiredArtifactBytes int64
	Nodes                 []TieredNode
	Controller            ModelController
	Now                   func() time.Time
}

type TieredRequestRouter struct {
	mu                    sync.Mutex
	modelID               string
	catalogVersion        string
	artifactSHA256        string
	requiredCapacityBytes int64
	requiredArtifactBytes int64
	controller            ModelController
	now                   func() time.Time
	nodes                 map[string]*tieredNode
	closed                bool
}

type tieredNode struct {
	status     NodeStatus
	provider   *Provider
	active     map[*tieredAttempt]context.CancelFunc
	loading    bool
	warming    bool
	loadToken  uint64
	warmToken  uint64
	loadCancel context.CancelFunc
	warmCancel context.CancelFunc
}

func NewTieredRequestRouter(config TieredRouterConfig) (*TieredRequestRouter, error) {
	if config.ModelID != api.ModelID || !validUUID(config.CatalogVersion) || !validSHA256(config.ArtifactSHA256) ||
		config.RequiredCapacityBytes <= 0 || config.RequiredArtifactBytes <= 0 || len(config.Nodes) == 0 {
		return nil, errors.New("tiered router requires a verified model and bounded node catalog")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	router := &TieredRequestRouter{modelID: config.ModelID, catalogVersion: config.CatalogVersion,
		artifactSHA256: config.ArtifactSHA256, requiredCapacityBytes: config.RequiredCapacityBytes,
		requiredArtifactBytes: config.RequiredArtifactBytes, controller: config.Controller,
		now: config.Now, nodes: make(map[string]*tieredNode, len(config.Nodes))}
	for _, candidate := range config.Nodes {
		if candidate.Provider == nil || validateNodeStatus(candidate.Status) != nil ||
			candidate.Status.CatalogVersion != config.CatalogVersion {
			return nil, errors.New("tiered router contains invalid node metadata")
		}
		if _, exists := router.nodes[candidate.Status.NodeID]; exists {
			return nil, errors.New("tiered router contains duplicate node IDs")
		}
		router.nodes[candidate.Status.NodeID] = &tieredNode{status: candidate.Status, provider: candidate.Provider,
			active: make(map[*tieredAttempt]context.CancelFunc)}
	}
	return router, nil
}

var _ requestcoord.Router = (*TieredRequestRouter)(nil)

func (r *TieredRequestRouter) Acquire(ctx context.Context, modelID string, excluded map[string]struct{}, deadline time.Time) (requestcoord.Attempt, error) {
	if ctx == nil || modelID != r.modelID {
		return nil, requestcoord.ErrModelNotFound
	}
	if !deadline.After(r.now()) {
		return nil, requestcoord.ErrNoCapacity
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, requestcoord.ErrNoCapacity
	}
	loadedIdle, loadedBusy, onDisk := r.candidatesLocked(excluded)
	if len(loadedIdle) > 0 {
		attempt := r.claimLocked(loadedIdle[0])
		r.mu.Unlock()
		return attempt, nil
	}
	if len(loadedBusy) > 0 {
		attempt := r.claimLocked(loadedBusy[0])
		r.mu.Unlock()
		return attempt, nil
	}
	r.mu.Unlock()

	if len(onDisk) > 0 && r.controller != nil {
		for _, candidate := range onDisk {
			attempt, err := r.loadAndClaim(ctx, candidate, deadline)
			if err == nil {
				return attempt, nil
			}
			if errors.Is(err, requestcoord.ErrModelWarming) {
				return nil, err
			}
			if ctx.Err() != nil || !deadline.After(r.now()) {
				return nil, requestcoord.ErrNoCapacity
			}
		}
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, requestcoord.ErrNoCapacity
	}
	preparing := r.hasPreparationLocked(excluded)
	var jobs []warmJob
	if !preparing {
		jobs = r.markWarmLocked(r.absentCandidatesLocked(excluded))
	}
	r.mu.Unlock()
	if len(jobs) > 0 {
		for _, job := range jobs {
			go r.warmNode(job.node, job.generation, job.sessionID, job.token)
		}
		return nil, requestcoord.ErrModelWarming
	}
	if preparing {
		return nil, requestcoord.ErrModelWarming
	}
	return nil, requestcoord.ErrNoCapacity
}

func (r *TieredRequestRouter) candidatesLocked(excluded map[string]struct{}) (idle, busy, onDisk []*tieredNode) {
	now := r.now()
	for id, node := range r.nodes {
		if _, skip := excluded[id]; skip || !r.baseEligible(node.status, now) {
			continue
		}
		active := len(node.active)
		switch node.status.ModelState {
		case ModelLoaded:
			if node.status.ArtifactSHA256 != r.artifactSHA256 {
				continue
			}
			if active == 0 {
				idle = append(idle, node)
			} else if active < node.status.VerifiedSlots {
				busy = append(busy, node)
			}
		case ModelOnDisk:
			if active == 0 && !node.loading && !node.warming && node.status.ArtifactSHA256 == r.artifactSHA256 &&
				node.status.VerifiedCapacityBytes >= r.requiredCapacityBytes {
				onDisk = append(onDisk, node)
			}
		}
	}
	rankNodes(idle)
	rankNodes(busy)
	rankNodes(onDisk)
	return idle, busy, onDisk
}

func (r *TieredRequestRouter) absentCandidatesLocked(excluded map[string]struct{}) []*tieredNode {
	var candidates []*tieredNode
	now := r.now()
	warmSlots := maxConcurrentWarmups
	for id, node := range r.nodes {
		if node.warming {
			warmSlots--
		}
		if _, skip := excluded[id]; skip || !r.baseEligible(node.status, now) || node.status.ModelState != ModelAbsent ||
			len(node.active) != 0 || node.loading || node.warming || node.status.VerifiedCapacityBytes < r.requiredCapacityBytes ||
			node.status.AvailableDiskBytes < r.requiredArtifactBytes {
			continue
		}
		candidates = append(candidates, node)
	}
	if warmSlots <= 0 {
		return nil
	}
	rankNodes(candidates)
	if len(candidates) > warmSlots {
		candidates = candidates[:warmSlots]
	}
	return candidates
}

func (r *TieredRequestRouter) hasPreparationLocked(excluded map[string]struct{}) bool {
	now := r.now()
	for id, node := range r.nodes {
		if _, skip := excluded[id]; skip || !r.baseEligible(node.status, now) || len(node.active) != 0 ||
			node.status.VerifiedCapacityBytes < r.requiredCapacityBytes {
			continue
		}
		if node.loading && node.status.ModelState == ModelOnDisk && node.status.ArtifactSHA256 == r.artifactSHA256 {
			return true
		}
		if node.warming && node.status.ModelState == ModelAbsent && node.status.AvailableDiskBytes >= r.requiredArtifactBytes {
			return true
		}
	}
	return false
}

func (r *TieredRequestRouter) baseEligible(status NodeStatus, now time.Time) bool {
	return status.Online && !status.Quarantined && !status.Held && status.CatalogVersion == r.catalogVersion &&
		!status.LastHeartbeat.IsZero() && !status.LastHeartbeat.After(now) && now.Sub(status.LastHeartbeat) < maxNodeHeartbeatAge &&
		!status.BenchmarkedAt.IsZero() && !status.BenchmarkedAt.After(now) && now.Sub(status.BenchmarkedAt) < maxBenchmarkAge &&
		status.VerifiedSlots > 0
}

func (r *TieredRequestRouter) claimLocked(node *tieredNode) *tieredAttempt {
	attempt := &tieredAttempt{router: r, node: node, ownerID: node.status.NodeID,
		generation: node.status.Generation, sessionID: node.status.SessionID}
	node.active[attempt] = nil
	return attempt
}

// UpdateStatus replaces the server-observed node state. A higher generation
// fences every lease from the prior WebSocket session before it can complete.
func (r *TieredRequestRouter) UpdateStatus(status NodeStatus) error {
	if err := validateNodeStatus(status); err != nil || status.CatalogVersion != r.catalogVersion {
		return errors.New("invalid node session status")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	node, exists := r.nodes[status.NodeID]
	if !exists || status.Generation < node.status.Generation ||
		status.Generation == node.status.Generation && status.SessionID != node.status.SessionID ||
		status.Generation == node.status.Generation && status.LastHeartbeat.Before(node.status.LastHeartbeat) ||
		status.Generation == node.status.Generation && !node.status.Online && status.Online {
		return errors.New("stale node session")
	}
	if status.Generation > node.status.Generation || !status.Online {
		r.fenceNodeLocked(node)
	}
	node.status = status
	return nil
}

func (r *TieredRequestRouter) fenceNodeLocked(node *tieredNode) {
	for _, cancel := range node.active {
		if cancel != nil {
			cancel()
		}
	}
	node.active = make(map[*tieredAttempt]context.CancelFunc)
	if node.loadCancel != nil {
		node.loadCancel()
	}
	node.loadToken++
	if node.warmCancel != nil {
		node.warmCancel()
	}
	node.warmToken++
	node.loading, node.warming = false, false
}

func (r *TieredRequestRouter) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	for _, node := range r.nodes {
		r.fenceNodeLocked(node)
	}
}

func rankNodes(nodes []*tieredNode) {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].status.Rank != nodes[j].status.Rank {
			return nodes[i].status.Rank > nodes[j].status.Rank
		}
		if nodes[i].status.FirstTokenLatency != nodes[j].status.FirstTokenLatency {
			return nodes[i].status.FirstTokenLatency < nodes[j].status.FirstTokenLatency
		}
		return nodes[i].status.NodeID < nodes[j].status.NodeID
	})
	for start := 0; start < len(nodes); {
		end := start + 1
		for end < len(nodes) && nodes[end].status.Rank == nodes[start].status.Rank &&
			nodes[end].status.FirstTokenLatency == nodes[start].status.FirstTokenLatency {
			end++
		}
		if end-start > 1 {
			rand.Shuffle(end-start, func(i, j int) { nodes[start+i], nodes[start+j] = nodes[start+j], nodes[start+i] })
		}
		start = end
	}
}

func validateNodeStatus(status NodeStatus) error {
	if !validUUID(status.NodeID) || !validUUID(status.SessionID) || status.Generation == 0 ||
		!validUUID(status.CatalogVersion) || status.VerifiedSlots < 0 || status.VerifiedSlots > 64 ||
		status.VerifiedCapacityBytes < 0 || status.AvailableDiskBytes < 0 || status.Rank < 0 || status.Rank > 100 ||
		status.FirstTokenLatency < 0 || (status.ModelState != ModelLoaded && status.ModelState != ModelOnDisk && status.ModelState != ModelAbsent) {
		return errors.New("invalid node status")
	}
	if status.ModelState != ModelAbsent && !validSHA256(status.ArtifactSHA256) {
		return errors.New("verified model state requires a SHA-256 artifact hash")
	}
	if status.Online && status.LastHeartbeat.IsZero() {
		return errors.New("online node status requires a heartbeat")
	}
	return nil
}

func validUUID(value string) bool {
	_, err := domain.ParseID(value)
	return err == nil
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}
