package c3route

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"cordova/server/internal/domain"
	"cordova/server/internal/inference"
	requestcoord "cordova/server/internal/request"
)

const (
	defaultAcceptTimeout = 2 * time.Second
	defaultCancelTimeout = 2 * time.Second
	defaultMaxOutput     = 1 << 20
	maxDeltaBytes        = 16 << 10
)

// Config tunes the router. Zero timeouts use C3 defaults.
type Config struct {
	Encoder        Encoder
	AcceptTimeout  time.Duration // inference.accepted deadline (C3: 2s)
	CancelTimeout  time.Duration // wait for inference.cancelled before giving up
	MaxOutputBytes int
}

// NodeInfo describes a freshly authenticated session.
type NodeInfo struct {
	NodeID         string
	Session        Session
	CatalogVersion string
	Models         []string // models reported loaded and verified
}

// Router is a pilot router: one slot per node, no queueing. It is process-local
// and does not use Redis leases.
type Router struct {
	cfg   Config
	mu    sync.Mutex
	nodes map[string]*nodeEntry
}

type nodeEntry struct {
	info   NodeInfo
	gen    uint64
	models map[string]struct{}
	busy   *attempt
	fenced chan struct{}
}

var _ requestcoord.Router = (*Router)(nil)

func New(cfg Config) (*Router, error) {
	if cfg.Encoder == nil {
		return nil, errors.New("c3route requires an input encoder")
	}
	if cfg.AcceptTimeout <= 0 {
		cfg.AcceptTimeout = defaultAcceptTimeout
	}
	if cfg.CancelTimeout <= 0 {
		cfg.CancelTimeout = defaultCancelTimeout
	}
	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = defaultMaxOutput
	}
	return &Router{cfg: cfg, nodes: make(map[string]*nodeEntry)}, nil
}

// Register installs a session. A strictly newer generation replaces and fences
// the previous session of the same node, including its active attempt.
func (r *Router) Register(info NodeInfo) error {
	if info.Session == nil {
		return errors.New("c3route session is required")
	}
	if _, err := domain.ParseID(info.NodeID); err != nil {
		return errors.New("c3route node ID is invalid")
	}
	gen := info.Session.Generation()
	entry := &nodeEntry{info: info, gen: gen, models: make(map[string]struct{}, len(info.Models)), fenced: make(chan struct{})}
	for _, m := range info.Models {
		entry.models[m] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old := r.nodes[info.NodeID]; old != nil {
		if gen <= old.gen {
			return errors.New("c3route session generation is not newer")
		}
		close(old.fenced)
	}
	r.nodes[info.NodeID] = entry
	return nil
}

// Unregister removes the session of that generation (no-op if superseded).
func (r *Router) Unregister(nodeID string, generation uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.nodes[nodeID]; e != nil && e.gen == generation {
		close(e.fenced)
		delete(r.nodes, nodeID)
	}
}

// Acquire claims the single slot of the first eligible node (sorted by ID).
// A busy or absent node is skipped; with none eligible it returns ErrNoCapacity
// immediately.
func (r *Router) Acquire(ctx context.Context, modelID string, excluded map[string]struct{}, deadline time.Time) (requestcoord.Attempt, error) {
	if ctx == nil {
		return nil, requestcoord.ErrNoCapacity
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if modelID == "" || !deadline.After(time.Now()) {
		return nil, requestcoord.ErrNoCapacity
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.nodes))
	for id := range r.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, skip := excluded[id]; skip {
			continue
		}
		e := r.nodes[id]
		if _, ok := e.models[modelID]; !ok || e.busy != nil {
			continue
		}
		leaseID, err1 := newUUID()
		attemptID, err2 := newUUID()
		if err1 != nil || err2 != nil {
			return nil, requestcoord.ErrNoCapacity
		}
		a := &attempt{router: r, entry: e, modelID: modelID, leaseID: leaseID, attemptID: attemptID, deadline: deadline}
		e.busy = a
		return a, nil
	}
	return nil, requestcoord.ErrNoCapacity
}

type attempt struct {
	router    *Router
	entry     *nodeEntry
	modelID   string
	leaseID   string
	attemptID string
	deadline  time.Time

	mu      sync.Mutex
	started bool
	once    sync.Once
}

var _ requestcoord.Attempt = (*attempt)(nil)
var _ requestcoord.AcceptedAttempt = (*attempt)(nil)

func (a *attempt) OwnerID() string { return a.entry.info.NodeID }

func (a *attempt) Run(ctx context.Context, in inference.Request, emit func(string) error) (string, error) {
	return a.RunAccepted(ctx, in, nil, emit)
}

// Release frees the slot exactly once. It never frees a newer session's slot.
func (a *attempt) Release(context.Context) {
	a.once.Do(func() {
		r := a.router
		r.mu.Lock()
		if a.entry.busy == a {
			a.entry.busy = nil
		}
		r.mu.Unlock()
	})
}

func (a *attempt) fencedNow() bool {
	select {
	case <-a.entry.fenced:
		return true
	default:
		return false
	}
}

func (a *attempt) RunAccepted(ctx context.Context, in inference.Request, accepted func() error, emit func(string) error) (string, error) {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return "", requestcoord.ErrNodeFailed
	}
	a.started = true
	a.mu.Unlock()
	if a.fencedNow() || ctx.Err() != nil || !a.deadline.After(time.Now()) {
		return "", requestcoord.ErrNodeFailed
	}
	cfg := a.router.cfg
	runCtx, cancel := context.WithDeadline(ctx, a.deadline)
	defer cancel()

	ids, err := cfg.Encoder.EncodeInput(runCtx, a.modelID, in.Messages)
	if err != nil || len(ids) == 0 {
		return "", requestcoord.ErrNodeFailed
	}
	frame := StartFrame{AttemptID: a.attemptID, LeaseID: a.leaseID, ModelID: a.modelID,
		CatalogVersion: a.entry.info.CatalogVersion, InputTokenIDs: ids, InputTokens: len(ids),
		MaxTokens: in.MaxTokens, Temperature: in.Parameters.Temperature, TopP: in.Parameters.TopP,
		Stop: append([]string(nil), in.Parameters.Stop...), Seed: in.Parameters.Seed,
		DeadlineAt: a.deadline.UTC().Format(time.RFC3339), Kind: "user"}
	stream, err := a.entry.info.Session.Start(runCtx, frame)
	if err != nil || stream == nil {
		return "", requestcoord.ErrNodeFailed
	}
	defer stream.Close()
	events := stream.Events()

	abort := func(reason string, result error) (string, error) {
		a.cancelAndWait(stream, reason)
		return "", result
	}
	ctxReason := func() (string, error) {
		if errors.Is(ctx.Err(), context.Canceled) {
			_, err := abort(ReasonClientCancelled, nil)
			_ = err
			return "", ctx.Err()
		}
		return abort(ReasonDeadline, requestcoord.ErrNodeFailed)
	}

	acceptTimer := time.NewTimer(cfg.AcceptTimeout)
	defer acceptTimer.Stop()
	var (
		isAccepted bool
		next       int64
		total      int
	)
	for {
		var ev Event
		var ok bool
		select {
		case ev, ok = <-events:
			if !ok {
				return "", requestcoord.ErrNodeFailed
			}
		case <-runCtx.Done():
			return ctxReason()
		case <-a.entry.fenced:
			return abort(ReasonSuperseded, requestcoord.ErrNodeFailed)
		case <-acceptTimer.C:
			if isAccepted {
				acceptTimer.Reset(time.Hour)
				continue
			}
			return abort(ReasonDeadline, requestcoord.ErrNodeFailed)
		}
		if ev.Generation != a.entry.gen {
			continue // late frame from another session generation
		}
		if a.fencedNow() {
			return abort(ReasonSuperseded, requestcoord.ErrNodeFailed)
		}
		if ev.AttemptID != a.attemptID || ev.LeaseID != a.leaseID {
			return abort(ReasonSuperseded, requestcoord.ErrNodeFailed)
		}
		switch ev.Type {
		case EventAccepted:
			if isAccepted {
				return abort(ReasonSuperseded, requestcoord.ErrNodeFailed)
			}
			isAccepted = true
			if accepted != nil {
				if err := accepted(); err != nil {
					return abort(ReasonClientCancelled, err)
				}
			}
		case EventDelta:
			if !isAccepted || ev.ChunkIndex != next || len(ev.Text) > maxDeltaBytes || !utf8.ValidString(ev.Text) {
				return abort(ReasonSuperseded, requestcoord.ErrNodeFailed)
			}
			if total+len(ev.Text) > cfg.MaxOutputBytes {
				return abort(ReasonOutputLimit, requestcoord.ErrOutputLimit)
			}
			total += len(ev.Text)
			next++
			if err := emit(ev.Text); err != nil {
				return abort(ReasonClientCancelled, err)
			}
		case EventComplete:
			if !isAccepted || (ev.FinishReason != "stop" && ev.FinishReason != "length") {
				return abort(ReasonSuperseded, requestcoord.ErrNodeFailed)
			}
			if (next == 0 && ev.LastChunkIndex != nil) || (next > 0 && (ev.LastChunkIndex == nil || *ev.LastChunkIndex != next-1)) {
				return abort(ReasonSuperseded, requestcoord.ErrNodeFailed)
			}
			return ev.FinishReason, nil
		default: // error, unsolicited cancelled, unknown
			return "", fmt.Errorf("%w: node frame %q %s", requestcoord.ErrNodeFailed, ev.Type, ev.Code)
		}
	}
}

// cancelAndWait sends inference.cancel and waits a bounded time for the node's
// acknowledgement (or session end) so the node slot is really free.
func (a *attempt) cancelAndWait(stream Stream, reason string) {
	cfg := a.router.cfg
	ctx, cancel := context.WithTimeout(context.Background(), cfg.CancelTimeout)
	defer cancel()
	if err := stream.Cancel(ctx, CancelFrame{AttemptID: a.attemptID, LeaseID: a.leaseID, Reason: reason}); err != nil {
		return
	}
	for {
		select {
		case ev, ok := <-stream.Events():
			if !ok || (ev.Generation == a.entry.gen && ev.AttemptID == a.attemptID && ev.LeaseID == a.leaseID &&
				(ev.Type == EventCancelled || ev.Type == EventError || ev.Type == EventComplete)) {
				return
			}
		case <-a.entry.fenced:
			return
		case <-ctx.Done():
			return
		}
	}
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
