package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cordova/server/internal/inference"
)

type waitForRequestCancellation struct {
	started chan struct{}
	calls   atomic.Int32
}

func (e *waitForRequestCancellation) Execute(ctx context.Context, _ CompletionRequest, _ func(string) error) (CompletionResult, *CompletionFailure) {
	e.calls.Add(1)
	close(e.started)
	<-ctx.Done()
	return CompletionResult{Status: "cancelled", Usage: inference.Usage{PromptTokens: 1, TotalTokens: 1}},
		&CompletionFailure{StatusCode: http.StatusRequestTimeout, Code: "timeout", Message: "The request was cancelled."}
}

func TestKeyRevocationCancelsActiveCompletionAndFencesNewOnes(t *testing.T) {
	registry := NewRequestRegistry()
	executor := &waitForRequestCancellation{started: make(chan struct{})}
	handler, err := NewWithAuthAndExecutorRuntime(executor, func(*http.Request) (Principal, bool) {
		return Principal{AccountID: "00000000-0000-4000-8000-000000000001", KeyID: "00000000-0000-4000-8000-000000000002",
			Kind: "key", Scopes: map[string]bool{"chat:write": true}, AllowedModels: map[string]bool{ModelID: true}}, true
	}, nil, nil, registry)
	if err != nil {
		t.Fatal(err)
	}

	firstRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	firstRequest.Header.Set("Content-Type", "application/json")
	firstResponse := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(firstResponse, firstRequest)
		close(firstDone)
	}()
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("completion did not start")
	}

	registry.CancelKey("00000000-0000-4000-8000-000000000002")
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("revocation did not cancel the active completion")
	}
	if firstResponse.Code != http.StatusRequestTimeout {
		t.Fatalf("cancelled completion status=%d body=%s", firstResponse.Code, firstResponse.Body.String())
	}

	secondRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	secondRequest.Header.Set("Content-Type", "application/json")
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, secondRequest)
	if secondResponse.Code != http.StatusUnauthorized || executor.calls.Load() != 1 {
		t.Fatalf("revoked key was admitted again: status=%d calls=%d body=%s", secondResponse.Code, executor.calls.Load(), secondResponse.Body.String())
	}
}
