package mocknode

import (
	"context"
	"sync"

	"cordova/server/internal/inference"
	requestcoord "cordova/server/internal/request"
)

type tieredAttempt struct {
	router     *TieredRequestRouter
	node       *tieredNode
	ownerID    string
	generation uint64
	sessionID  string
	once       sync.Once
}

var _ requestcoord.Attempt = (*tieredAttempt)(nil)
var _ requestcoord.AcceptedAttempt = (*tieredAttempt)(nil)

func (a *tieredAttempt) OwnerID() string { return a.ownerID }

func (a *tieredAttempt) Run(ctx context.Context, input inference.Request, emit func(string) error) (string, error) {
	return a.RunAccepted(ctx, input, nil, emit)
}

func (a *tieredAttempt) RunAccepted(ctx context.Context, input inference.Request, accepted func() error,
	emit func(string) error) (string, error) {
	if ctx == nil || !a.router.current(a) {
		return "", requestcoord.ErrNodeFailed
	}
	runCtx, cancel := context.WithCancel(ctx)
	a.router.mu.Lock()
	if !a.router.currentLocked(a) {
		a.router.mu.Unlock()
		cancel()
		return "", requestcoord.ErrNodeFailed
	}
	a.node.active[a] = cancel
	a.router.mu.Unlock()
	defer cancel()
	result, err := a.node.provider.GenerateAccepted(runCtx, input, accepted, func(text string) error {
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

func (r *TieredRequestRouter) current(attempt *tieredAttempt) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentLocked(attempt)
}

func (r *TieredRequestRouter) currentLocked(attempt *tieredAttempt) bool {
	_, active := attempt.node.active[attempt]
	return !r.closed && active && attempt.node.status.Online && attempt.node.status.Generation == attempt.generation &&
		attempt.node.status.SessionID == attempt.sessionID
}

func (a *tieredAttempt) Release(context.Context) {
	a.once.Do(func() {
		a.router.mu.Lock()
		cancel := a.node.active[a]
		delete(a.node.active, a)
		a.router.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	})
}
