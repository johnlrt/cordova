package api

import (
	"context"
	"sync"
)

// RequestRegistry ties active completion contexts to API-key revocation. Key
// IDs are immutable UUIDs, so a revoked entry never needs to be cleared.
type RequestRegistry struct {
	mu      sync.Mutex
	active  map[string]map[string]context.CancelFunc
	revoked map[string]struct{}
}

func NewRequestRegistry() *RequestRegistry {
	return &RequestRegistry{active: make(map[string]map[string]context.CancelFunc), revoked: make(map[string]struct{})}
}

func (r *RequestRegistry) Register(keyID, requestID string, cancel context.CancelFunc) bool {
	if r == nil || keyID == "" || requestID == "" || cancel == nil {
		return false
	}
	r.mu.Lock()
	if _, revoked := r.revoked[keyID]; revoked {
		r.mu.Unlock()
		cancel()
		return false
	}
	if r.active[keyID] == nil {
		r.active[keyID] = make(map[string]context.CancelFunc)
	}
	r.active[keyID][requestID] = cancel
	r.mu.Unlock()
	return true
}

func (r *RequestRegistry) Unregister(keyID, requestID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if requests := r.active[keyID]; requests != nil {
		delete(requests, requestID)
		if len(requests) == 0 {
			delete(r.active, keyID)
		}
	}
	r.mu.Unlock()
}

// CancelKey permanently fences future registrations and cancels every active
// request that authenticated with the revoked key before the revocation won.
func (r *RequestRegistry) CancelKey(keyID string) {
	if r == nil || keyID == "" {
		return
	}
	r.mu.Lock()
	r.revoked[keyID] = struct{}{}
	cancels := make([]context.CancelFunc, 0, len(r.active[keyID]))
	for _, cancel := range r.active[keyID] {
		cancels = append(cancels, cancel)
	}
	r.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}
