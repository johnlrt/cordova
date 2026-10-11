package mocknode

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"cordova/server/internal/api"
	"cordova/server/internal/inference"
	requestcoord "cordova/server/internal/request"
)

const testCatalogVersion = "00000000-0000-4000-8000-000000000099"

var testArtifactHash = strings.Repeat("a", 64)

type tieredControllerStub struct {
	mu              sync.Mutex
	states          map[string]RemoteModelStatus
	queries         []string
	loads           []string
	downloads       []string
	queryErr        error
	loadErr         error
	downloadStarted chan string
	downloadRelease <-chan struct{}
}

func (c *tieredControllerStub) Query(_ context.Context, nodeID, _, _ string) (RemoteModelStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queries = append(c.queries, nodeID)
	return c.states[nodeID], c.queryErr
}

func (c *tieredControllerStub) Load(_ context.Context, nodeID, _, _ string) (string, error) {
	c.mu.Lock()
	c.loads = append(c.loads, nodeID)
	err := c.loadErr
	c.mu.Unlock()
	if err != nil {
		return "", err
	}
	return testArtifactHash, nil
}

func (c *tieredControllerStub) Download(ctx context.Context, nodeID, _, _ string) (string, error) {
	c.mu.Lock()
	c.downloads = append(c.downloads, nodeID)
	c.mu.Unlock()
	if c.downloadStarted != nil {
		c.downloadStarted <- nodeID
	}
	if c.downloadRelease != nil {
		select {
		case <-c.downloadRelease:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return testArtifactHash, nil
}

func (c *tieredControllerStub) counts() (queries, loads, downloads int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queries), len(c.loads), len(c.downloads)
}

func newTieredRouterForTest(t *testing.T, statuses []NodeStatus, controller ModelController) (*TieredRequestRouter, time.Time) {
	t.Helper()
	now := time.Now().UTC()
	nodes := make([]TieredNode, len(statuses))
	for i, status := range statuses {
		nodes[i] = TieredNode{Status: status, Provider: &Provider{}}
	}
	router, err := NewTieredRequestRouter(TieredRouterConfig{
		ModelID: api.ModelID, CatalogVersion: testCatalogVersion, ArtifactSHA256: testArtifactHash,
		RequiredCapacityBytes: 512, RequiredArtifactBytes: 1024, Nodes: nodes, Controller: controller,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewTieredRequestRouter() error = %v", err)
	}
	t.Cleanup(router.Close)
	return router, now
}

func tieredStatus(n int, state ModelResidency, rank, slots int) NodeStatus {
	status := NodeStatus{
		NodeID: testNodeID(n), SessionID: testNodeID(n + 100), Generation: 1, Online: true,
		LastHeartbeat: time.Now().UTC(), CatalogVersion: testCatalogVersion, ModelState: state,
		BenchmarkedAt: time.Now().UTC().Add(-time.Minute), BenchmarkPassed: true, VerifiedSlots: slots,
		VerifiedCapacityBytes: 2048, AvailableDiskBytes: 4096, Rank: rank,
		FirstTokenLatency: 100 * time.Millisecond,
	}
	if state != ModelAbsent {
		status.ArtifactSHA256 = testArtifactHash
	}
	return status
}

func testNodeID(n int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", n)
}

func acquireTiered(t *testing.T, router *TieredRequestRouter, excluded map[string]struct{}) requestcoord.Attempt {
	t.Helper()
	attempt, err := router.Acquire(context.Background(), api.ModelID, excluded, time.Now().Add(30*time.Second))
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	return attempt
}

func TestTieredRouterPrefersLoadedIdleThenRankedBusyCapacity(t *testing.T) {
	statuses := []NodeStatus{
		tieredStatus(1, ModelLoaded, 10, 2),
		tieredStatus(2, ModelLoaded, 100, 2),
		tieredStatus(3, ModelLoaded, 20, 2),
	}
	router, _ := newTieredRouterForTest(t, statuses, nil)

	busy := acquireTiered(t, router, map[string]struct{}{testNodeID(1): {}, testNodeID(3): {}})
	if got := busy.OwnerID(); got != testNodeID(2) {
		t.Fatalf("forced busy lease owner = %s, want node 2", got)
	}
	idle := acquireTiered(t, router, nil)
	if got := idle.OwnerID(); got != testNodeID(3) {
		t.Fatalf("idle priority/rank owner = %s, want highest-ranked idle node 3", got)
	}
	secondBusySlot := acquireTiered(t, router, map[string]struct{}{testNodeID(1): {}, testNodeID(3): {}})
	if got := secondBusySlot.OwnerID(); got != testNodeID(2) {
		t.Fatalf("busy fallback owner = %s, want node 2", got)
	}
	if _, err := router.Acquire(context.Background(), api.ModelID,
		map[string]struct{}{testNodeID(1): {}, testNodeID(3): {}}, time.Now().Add(time.Second)); !errors.Is(err, requestcoord.ErrNoCapacity) {
		t.Fatalf("full node admitted another lease: %v", err)
	}
	busy.Release(context.Background())
	idle.Release(context.Background())
	secondBusySlot.Release(context.Background())
}

func TestTieredRouterLoadsVerifiedOnDiskModelBeforeClaim(t *testing.T) {
	status := tieredStatus(4, ModelOnDisk, 50, 2)
	controller := &tieredControllerStub{states: map[string]RemoteModelStatus{
		status.NodeID: {State: ModelOnDisk, ArtifactSHA256: testArtifactHash},
	}}
	router, _ := newTieredRouterForTest(t, []NodeStatus{status}, controller)

	attempt := acquireTiered(t, router, nil)
	if attempt.OwnerID() != status.NodeID {
		t.Fatalf("owner = %s, want %s", attempt.OwnerID(), status.NodeID)
	}
	queries, loads, downloads := controller.counts()
	if queries != 1 || loads != 1 || downloads != 0 {
		t.Fatalf("controller calls query/load/download = %d/%d/%d", queries, loads, downloads)
	}
	attempt.Release(context.Background())
}

func TestTieredRouterAllowsAbsentQueryWithoutHashAndReturnsWarming(t *testing.T) {
	status := tieredStatus(5, ModelOnDisk, 50, 2)
	// Hold the background download so its later Load call cannot race this assertion.
	downloadRelease := make(chan struct{})
	controller := &tieredControllerStub{states: map[string]RemoteModelStatus{
		status.NodeID: {State: ModelAbsent},
	}, downloadStarted: make(chan string, 1), downloadRelease: downloadRelease}
	router, _ := newTieredRouterForTest(t, []NodeStatus{status}, controller)

	if _, err := router.Acquire(context.Background(), api.ModelID, nil, time.Now().Add(time.Minute)); !errors.Is(err, requestcoord.ErrModelWarming) {
		t.Fatalf("Acquire() error = %v, want model warming", err)
	}
	queries, loads, _ := controller.counts()
	if queries != 1 || loads != 0 {
		t.Fatalf("controller calls query/load = %d/%d, want 1/0", queries, loads)
	}
	select {
	case nodeID := <-controller.downloadStarted:
		if nodeID != status.NodeID {
			t.Fatalf("download started for %s, want %s", nodeID, status.NodeID)
		}
	case <-time.After(time.Second):
		t.Fatal("absent model did not start background download")
	}
}

func TestTieredRouterLoadTimeoutReturnsWarmingAndDoesNotServeRequest(t *testing.T) {
	status := tieredStatus(6, ModelOnDisk, 50, 2)
	controller := &tieredControllerStub{states: map[string]RemoteModelStatus{
		status.NodeID: {State: ModelOnDisk, ArtifactSHA256: testArtifactHash},
	}, loadErr: context.DeadlineExceeded}
	router, _ := newTieredRouterForTest(t, []NodeStatus{status}, controller)

	if _, err := router.Acquire(context.Background(), api.ModelID, nil, time.Now().Add(time.Minute)); !errors.Is(err, requestcoord.ErrModelWarming) {
		t.Fatalf("Acquire() error = %v, want model warming", err)
	}
}

func TestTieredRouterQueryTimeoutDoesNotAssumeArtifactIsAbsent(t *testing.T) {
	status := tieredStatus(13, ModelOnDisk, 50, 2)
	controller := &tieredControllerStub{states: map[string]RemoteModelStatus{}, queryErr: context.DeadlineExceeded}
	router, _ := newTieredRouterForTest(t, []NodeStatus{status}, controller)

	if _, err := router.Acquire(context.Background(), api.ModelID, nil, time.Now().Add(time.Minute)); !errors.Is(err, requestcoord.ErrModelWarming) {
		t.Fatalf("Acquire() error = %v, want model warming", err)
	}
	if _, _, downloads := controller.counts(); downloads != 0 {
		t.Fatalf("query timeout incorrectly started %d downloads", downloads)
	}
}

func TestTieredRouterWarmsAtMostTwoAbsentNodesAcrossRequests(t *testing.T) {
	statuses := []NodeStatus{
		tieredStatus(7, ModelAbsent, 70, 2),
		tieredStatus(8, ModelAbsent, 60, 2),
		tieredStatus(9, ModelAbsent, 50, 2),
	}
	release := make(chan struct{})
	controller := &tieredControllerStub{downloadStarted: make(chan string, 4), downloadRelease: release}
	router, _ := newTieredRouterForTest(t, statuses, controller)
	defer close(release)

	for call := 0; call < 2; call++ {
		if _, err := router.Acquire(context.Background(), api.ModelID, nil, time.Now().Add(time.Minute)); !errors.Is(err, requestcoord.ErrModelWarming) {
			t.Fatalf("Acquire() call %d error = %v, want model warming", call+1, err)
		}
	}
	started := map[string]struct{}{}
	for len(started) < maxConcurrentWarmups {
		select {
		case nodeID := <-controller.downloadStarted:
			started[nodeID] = struct{}{}
		case <-time.After(time.Second):
			t.Fatalf("only %d warmups started, want %d", len(started), maxConcurrentWarmups)
		}
	}
	if len(started) != 2 {
		t.Fatalf("unique warmups = %d, want 2", len(started))
	}
	if _, err := router.Acquire(context.Background(), api.ModelID, nil, time.Now().Add(time.Minute)); !errors.Is(err, requestcoord.ErrModelWarming) {
		t.Fatalf("Acquire() during warmup error = %v, want model warming", err)
	}
	if _, _, downloads := controller.counts(); downloads != maxConcurrentWarmups {
		t.Fatalf("downloads after repeated requests = %d, want bounded at %d", downloads, maxConcurrentWarmups)
	}
}

func TestTieredRouterFencesAttemptOnNewSessionGeneration(t *testing.T) {
	status := tieredStatus(10, ModelLoaded, 50, 1)
	router, _ := newTieredRouterForTest(t, []NodeStatus{status}, nil)
	attempt := acquireTiered(t, router, nil)

	newStatus := status
	newStatus.Generation++
	newStatus.SessionID = testNodeID(110)
	newStatus.LastHeartbeat = time.Now().UTC()
	if err := router.UpdateStatus(newStatus); err != nil {
		t.Fatalf("UpdateStatus(new generation) error = %v", err)
	}
	if _, err := attempt.Run(context.Background(), inference.Request{}, nil); !errors.Is(err, requestcoord.ErrNodeFailed) {
		t.Fatalf("fenced attempt Run() error = %v, want node failure", err)
	}
	if err := router.UpdateStatus(status); err == nil {
		t.Fatal("stale generation update was accepted")
	}
	attempt.Release(context.Background())
}

func TestTieredRouterRejectsOutOfOrderHeartbeat(t *testing.T) {
	status := tieredStatus(14, ModelLoaded, 50, 1)
	router, _ := newTieredRouterForTest(t, []NodeStatus{status}, nil)
	stale := status
	stale.LastHeartbeat = status.LastHeartbeat.Add(-time.Second)
	if err := router.UpdateStatus(stale); err == nil {
		t.Fatal("out-of-order heartbeat rewound node freshness")
	}
}

func TestTieredRouterSkipsWrongCatalogAndArtifactWithoutRejectingInventory(t *testing.T) {
	good := tieredStatus(17, ModelLoaded, 10, 1)
	wrongCatalog := tieredStatus(18, ModelLoaded, 100, 1)
	wrongCatalog.CatalogVersion = "00000000-0000-4000-8000-000000000098"
	wrongArtifact := tieredStatus(19, ModelLoaded, 90, 1)
	wrongArtifact.ArtifactSHA256 = strings.Repeat("b", 64)
	router, _ := newTieredRouterForTest(t, []NodeStatus{good, wrongCatalog, wrongArtifact}, nil)

	attempt := acquireTiered(t, router, nil)
	if attempt.OwnerID() != good.NodeID {
		t.Fatalf("selected node %s, want the only catalog/hash match %s", attempt.OwnerID(), good.NodeID)
	}
	attempt.Release(context.Background())
	if _, err := router.Acquire(context.Background(), api.ModelID, map[string]struct{}{good.NodeID: {}}, time.Now().Add(time.Second)); !errors.Is(err, requestcoord.ErrNoCapacity) {
		t.Fatalf("wrong catalog/hash nodes supplied capacity: %v", err)
	}
}

func TestTieredRouterRejectsStaleAndHeldNodes(t *testing.T) {
	stale := tieredStatus(11, ModelLoaded, 100, 1)
	stale.LastHeartbeat = time.Now().Add(-maxNodeHeartbeatAge)
	held := tieredStatus(12, ModelLoaded, 100, 1)
	held.Held = true
	failedBenchmark := tieredStatus(15, ModelLoaded, 100, 1)
	failedBenchmark.BenchmarkPassed = false
	undersized := tieredStatus(16, ModelLoaded, 100, 1)
	undersized.VerifiedCapacityBytes = 128
	router, _ := newTieredRouterForTest(t, []NodeStatus{stale, held, failedBenchmark, undersized}, nil)
	if _, err := router.Acquire(context.Background(), api.ModelID, nil, time.Now().Add(time.Second)); !errors.Is(err, requestcoord.ErrNoCapacity) {
		t.Fatalf("Acquire() error = %v, want no capacity", err)
	}
}
