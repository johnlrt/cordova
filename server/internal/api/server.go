// Package api implements the deliberately limited stage-1 mock API.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"cordova/server/internal/domain"
	"cordova/server/internal/inference"
)

type Server struct {
	keyHash      [32]byte
	provider     inference.Provider
	slots        chan struct{}
	authenticate Authenticator
	usageSink    UsageSink
}

// Principal contains only identity and authorization data needed by a handler.
type Principal struct {
	AccountID     string
	Kind          string
	Scopes        map[string]bool
	KeyID         string
	AllowedModels map[string]bool
}

// Authenticator resolves an API key or a same-origin dashboard session.
type Authenticator func(*http.Request) (Principal, bool)

// UsageSink persists content-free request metadata before a successful response is completed.
type UsageSink func(context.Context, string, string, string, string, inference.Usage, time.Duration) error

func New(apiKey string, provider inference.Provider) (http.Handler, error) {
	if len(apiKey) < 32 || provider == nil {
		return nil, fmt.Errorf("API key must have at least 32 characters and provider must be configured")
	}
	s := &Server{keyHash: sha256.Sum256([]byte(apiKey)), provider: provider, slots: make(chan struct{}, 1)}
	s.authenticate = func(r *http.Request) (Principal, bool) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		provided := sha256.Sum256([]byte(token))
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || subtle.ConstantTimeCompare(provided[:], s.keyHash[:]) != 1 {
			return Principal{}, false
		}
		return Principal{AccountID: "00000000-0000-4000-8000-000000000001", Kind: "key", Scopes: map[string]bool{"models:read": true, "chat:write": true, "usage:read": true}, AllowedModels: map[string]bool{ModelID: true}}, true
	}
	return http.HandlerFunc(s.serve), nil
}

// NewWithAuth is the production composition point for account-scoped M1 handlers.
func NewWithAuth(provider inference.Provider, authenticate Authenticator, usageSink UsageSink) (http.Handler, error) {
	if provider == nil || authenticate == nil {
		return nil, fmt.Errorf("provider and authenticator must be configured")
	}
	// The single-host pilot serves one generation at a time in both standalone and M1 modes.
	s := &Server{provider: provider, slots: make(chan struct{}, 1), authenticate: authenticate, usageSink: usageSink}
	return http.HandlerFunc(s.serve), nil
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	requestIDValue, err := domain.NewID()
	if err != nil {
		http.Error(w, "request identifier unavailable", http.StatusServiceUnavailable)
		return
	}
	requestID := requestIDValue.String()
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("X-Cordova-Mode", "mock")
	w.Header().Set("X-Cordova-Usage", "synthetic-word-counts-not-billable")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "stage": "1", "mode": "mock"})
		return
	}
	if r.URL.Path != "/v1/models" && r.URL.Path != "/v1/chat/completions" {
		fail(w, 404, requestID, "not_found", "Unknown endpoint.")
		return
	}
	principal, ok := s.authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, 401, requestID, "unauthenticated", "A valid credential is required.")
		return
	}
	if r.URL.Path == "/v1/models" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			fail(w, 405, requestID, "invalid_request", "Use GET for this endpoint.")
			return
		}
		if principal.Kind != "session" && !principal.Scopes["models:read"] {
			fail(w, http.StatusForbidden, requestID, "forbidden", "This credential cannot read the model catalog.")
			return
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": []any{map[string]any{
			"id": ModelID, "object": "model", "created": 0, "owned_by": "cordova",
			"context_tokens": 8192, "max_output_tokens": 2048, "availability": "ready",
			"input_rate_piconero_per_million": "0", "output_rate_piconero_per_million": "0",
			"owner_share_bps": 0, "price_version": "00000000-0000-4000-8000-000000000001",
		}}})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		fail(w, 405, requestID, "invalid_request", "Use POST for this endpoint.")
		return
	}
	if principal.Kind != "key" || !principal.Scopes["chat:write"] {
		fail(w, http.StatusForbidden, requestID, "forbidden", "This credential cannot create chat completions.")
		return
	}
	if r.Header.Get("Idempotency-Key") != "" {
		fail(w, 400, requestID, "unsupported_parameter", "Idempotency storage is not implemented in stage 1.")
		return
	}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		w.Header().Set("Retry-After", "2")
		fail(w, 429, requestID, "rate_limited", "The demo concurrency limit has been reached.")
		return
	}
	s.chat(w, r, requestID, principal)
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func errorBody(requestID, code, message string, status int) errorResponse {
	kind := "invalid_request_error"
	switch {
	case status == 401:
		kind = "authentication_error"
	case status == 429:
		kind = "rate_limit_error"
	case status >= 500:
		kind = "server_error"
	}
	return errorResponse{Error: apiError{Message: message, Type: kind, Code: code}, RequestID: requestID}
}

func fail(w http.ResponseWriter, status int, id, code, message string) {
	writeJSON(w, status, errorBody(id, code, message, status))
}

func HTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
}
