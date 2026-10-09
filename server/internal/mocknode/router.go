package mocknode

import (
	"context"
	"errors"
	"sync"
	"time"

	"cordova/server/internal/api"
	"cordova/server/internal/domain"
	"cordova/server/internal/inference"
	requestcoord "cordova/server/internal/request"
)

// RequestRouter adapts the M1 WSS mock node to the M3 lease interface. It is
// intentionally single-owner and process-local; C3 authenticated node
// sessions and Redis slot leases are still required for M2 routing.
type RequestRouter struct {
	provider *Provider
	ownerID  string
	slots    chan struct{}

	mu         sync.Mutex
	generation uint64
	online     bool
	active     map[*providerAttempt]context.CancelFunc
}

func NewRequestRouter(provider *Provider, ownerID string, capacity int) (*RequestRouter, error) {
	if provider == nil || capacity < 1 || capacity > 64 {
		return nil, errors.New("mock request router requires a provider and bounded capacity")
	}
	if _, err := domain.ParseID(ownerID); err != nil {
		return nil, errors.New("mock request router owner ID is invalid")
	}
	return &RequestRouter{provider: provider, ownerID: ownerID, slots: make(chan struct{}, capacity), generation: 1,
		online: true, active: make(map[*providerAttempt]context.CancelFunc)}, nil
}

func (r *RequestRouter) Acquire(ctx context.Context, modelID string, excluded map[string]struct{}, deadline time.Time) (requestcoord.Attempt, error) {
	if ctx == nil || modelID != api.ModelID || !deadline.After(time.Now()) {
		return nil, requestcoord.ErrNoCapacity
	}
	if _, skip := excluded[r.ownerID]; skip {
		return nil, requestcoord.ErrNoCapacity
	}
	r.mu.Lock()
	if !r.online {
		r.mu.Unlock()
		return nil, requestcoord.ErrNoCapacity
	}
	generation := r.generation
	r.mu.Unlock()

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case r.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, requestcoord.ErrNoCapacity
	}
	lease := &providerAttempt{router: r, generation: generation}
	r.mu.Lock()
	if !r.online || r.generation != generation {
		r.mu.Unlock()
		<-r.slots
		return nil, requestcoord.ErrNoCapacity
	}
	r.active[lease] = nil
	r.mu.Unlock()
	return lease, nil
}

// Disconnect fences every current lease and cancels its connection. A later
// Reconnect starts a new generation; completions from older generations fail.
func (r *RequestRouter) Disconnect() {
	r.fence(false)
}

func (r *RequestRouter) Reconnect() {
	r.fence(true)
}

func (r *RequestRouter) fence(online bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.generation++
	r.online = online
	for lease, cancel := range r.active {
		if cancel != nil {
			cancel()
		}
		delete(r.active, lease)
	}
}

func (r *RequestRouter) current(lease *providerAttempt) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, active := r.active[lease]
	return active && r.online && r.generation == lease.generation
}

type providerAttempt struct {
	router     *RequestRouter
	generation uint64
	once       sync.Once
	cancel     context.CancelFunc
}

var _ requestcoord.Router = (*RequestRouter)(nil)
var _ requestcoord.Attempt = (*providerAttempt)(nil)
var _ requestcoord.AcceptedAttempt = (*providerAttempt)(nil)

func (a *providerAttempt) OwnerID() string { return a.router.ownerID }

func (a *providerAttempt) Run(ctx context.Context, input inference.Request, emit func(string) error) (string, error) {
	return a.RunAccepted(ctx, input, nil, emit)
}

func (a *providerAttempt) RunAccepted(ctx context.Context, input inference.Request, accepted func() error,
	emit func(string) error) (string, error) {
	if !a.router.current(a) {
		return "", requestcoord.ErrNodeFailed
	}
	runCtx, cancel := context.WithCancel(ctx)
	a.router.mu.Lock()
	if !a.router.online || a.router.generation != a.generation {
		a.router.mu.Unlock()
		cancel()
		return "", requestcoord.ErrNodeFailed
	}
	a.cancel = cancel
	a.router.active[a] = cancel
	a.router.mu.Unlock()
	defer cancel()
	result, err := a.router.provider.GenerateAccepted(runCtx, input, accepted, func(text string) error {
		if !a.router.current(a) {
			return requestcoord.ErrNodeFailed
		}
		return emit(text)
	})
	if err != nil {
		return "", err
	}
	if !a.router.current(a) {
		return "", requestcoord.ErrNodeFailed
	}
	return result.FinishReason, nil
}

func (a *providerAttempt) Release(context.Context) {
	a.once.Do(func() {
		a.router.mu.Lock()
		cancel := a.router.active[a]
		delete(a.router.active, a)
		a.router.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		<-a.router.slots
	})
}
