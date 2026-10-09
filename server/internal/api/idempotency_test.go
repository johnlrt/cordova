package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"cordova/server/internal/inference"
)

type idempotencyRecord struct {
	requestID string
	status    string
	body      string
}

type memoryIdempotency struct {
	mu      sync.Mutex
	records map[string]idempotencyRecord
}

func (m *memoryIdempotency) Claim(_ context.Context, _ Principal, key, _ string, requestID string,
	body []byte) (IdempotencyDecision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.records == nil {
		m.records = make(map[string]idempotencyRecord)
	}
	if record, ok := m.records[key]; ok {
		return IdempotencyDecision{RequestID: record.requestID, Status: record.status, Compared: true,
			BodyMatch: record.body == string(body)}, nil
	}
	m.records[key] = idempotencyRecord{requestID: requestID, status: "in_progress", body: string(body)}
	return IdempotencyDecision{Created: true, RequestID: requestID, Status: "in_progress", Compared: true, BodyMatch: true}, nil
}

func (m *memoryIdempotency) Complete(_ context.Context, _ Principal, key, requestID, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.records[key]
	if record.requestID != requestID {
		return ErrIdempotencyUnauthorized
	}
	record.status = status
	m.records[key] = record
	return nil
}

type settledExecutor struct{ calls int }

func (e *settledExecutor) Execute(_ context.Context, _ CompletionRequest, _ func(string) error) (CompletionResult, *CompletionFailure) {
	e.calls++
	return CompletionResult{Text: "ok", Usage: inference.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
		FinishReason: "stop", Status: "settled", Charged: "0"}, nil
}

func idempotencyHandler(t *testing.T, executor CompletionExecutor, manager IdempotencyManager) http.Handler {
	t.Helper()
	handler, err := NewWithAuthAndExecutorAndIdempotency(executor, func(*http.Request) (Principal, bool) {
		return Principal{AccountID: "00000000-0000-4000-8000-000000000001", KeyID: "00000000-0000-4000-8000-000000000002",
			Kind: "key", Scopes: map[string]bool{"chat:write": true}, AllowedModels: map[string]bool{ModelID: true}}, true
	}, nil, manager)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func idempotencyCall(handler http.Handler, body, key string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestIdempotencyPreventsReplayAndDetectsBodyMismatch(t *testing.T) {
	executor := &settledExecutor{}
	handler := idempotencyHandler(t, executor, &memoryIdempotency{})
	first := idempotencyCall(handler, validBody, "client-operation-1")
	if first.Code != http.StatusOK {
		t.Fatalf("first call failed: %d %s", first.Code, first.Body.String())
	}

	var replay struct {
		Error             apiError `json:"error"`
		RequestID         string   `json:"request_id"`
		OriginalRequestID string   `json:"original_request_id"`
	}
	second := idempotencyCall(handler, validBody, "client-operation-1")
	if second.Code != http.StatusConflict || json.Unmarshal(second.Body.Bytes(), &replay) != nil ||
		replay.Error.Code != "replay_unavailable" || replay.OriginalRequestID != first.Header().Get("X-Request-ID") || replay.RequestID == replay.OriginalRequestID {
		t.Fatalf("terminal replay was not safely rejected: %d %s", second.Code, second.Body.String())
	}

	different := strings.Replace(validBody, "Hello", "Different", 1)
	mismatch := idempotencyCall(handler, different, "client-operation-1")
	if mismatch.Code != http.StatusConflict || !strings.Contains(mismatch.Body.String(), `"code":"conflict"`) {
		t.Fatalf("different request body reused an idempotency key: %d %s", mismatch.Code, mismatch.Body.String())
	}
	if executor.calls != 1 {
		t.Fatalf("replay executed %d times", executor.calls)
	}
}

func TestIdempotencyRejectsConcurrentInProgressRequest(t *testing.T) {
	manager := &memoryIdempotency{records: map[string]idempotencyRecord{
		"still-running": {requestID: "00000000-0000-4000-8000-000000000099", status: "in_progress",
			body: `{"model":"cordova-chat-small-v1","messages":[{"role":"user","content":"Hello"}]}`},
	}}
	executor := &settledExecutor{}
	response := idempotencyCall(idempotencyHandler(t, executor, manager), validBody, "still-running")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"conflict"`) || executor.calls != 0 {
		t.Fatalf("in-progress request was replayed: status=%d calls=%d body=%s", response.Code, executor.calls, response.Body.String())
	}
}

func TestIdempotencyHeaderBounds(t *testing.T) {
	manager := &memoryIdempotency{}
	handler := idempotencyHandler(t, &settledExecutor{}, manager)
	if got := idempotencyCall(handler, validBody, strings.Repeat("a", 129)); got.Code != http.StatusBadRequest {
		t.Fatalf("oversized key status=%d body=%s", got.Code, got.Body.String())
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Add("Idempotency-Key", "one")
	request.Header.Add("Idempotency-Key", "two")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("duplicate header status=%d body=%s", response.Code, response.Body.String())
	}
}
