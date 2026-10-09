package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cordova/server/internal/inference"
)

type executorFunc func(context.Context, CompletionRequest, func(string) error) (CompletionResult, *CompletionFailure)

func (f executorFunc) Execute(ctx context.Context, request CompletionRequest, emit func(string) error) (CompletionResult, *CompletionFailure) {
	return f(ctx, request, emit)
}

func TestAccountedStreamStartsAfterAcceptanceAndSettlesBeforeDone(t *testing.T) {
	var accepted bool
	var persisted bool
	executor := executorFunc(func(_ context.Context, request CompletionRequest, emit func(string) error) (CompletionResult, *CompletionFailure) {
		if err := request.Accepted(); err != nil {
			return CompletionResult{Status: "cancelled"}, &CompletionFailure{StatusCode: 503, Code: "node_failed", Message: "failed"}
		}
		accepted = true
		if err := emit("hello"); err != nil {
			return CompletionResult{Status: "cancelled"}, &CompletionFailure{StatusCode: 503, Code: "node_failed", Message: "failed"}
		}
		return CompletionResult{Text: "hello", Usage: inference.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
			FinishReason: "stop", Status: "settled", Charged: "0"}, nil
	})
	handler, err := NewWithAuthAndExecutor(executor, testAuthenticator, func(_ context.Context, _, _, _, _, status, charged string, usage inference.Usage, _ time.Duration) error {
		if status != "settled" || charged != "0" || usage.TotalTokens != 2 {
			t.Fatalf("persisted status=%q charged=%q usage=%+v", status, charged, usage)
		}
		persisted = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"cordova-chat-small-v1","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`))
	request.Header.Set("Authorization", "Bearer test")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || !accepted || !persisted || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("response=%d headers=%v body=%s accepted=%v persisted=%v", response.Code, response.Header(), body, accepted, persisted)
	}
	role := strings.Index(body, `"role":"assistant"`)
	content := strings.Index(body, `"content":"hello"`)
	usage := strings.Index(body, `"total_tokens":2`)
	done := strings.Index(body, "data: [DONE]")
	if role < 0 || content < role || usage < content || done < usage {
		t.Fatalf("stream order is invalid: %s", body)
	}
}

func TestAccountedFailureBeforeAcceptanceRemainsHTTPError(t *testing.T) {
	handler, err := NewWithAuthAndExecutor(executorFunc(func(context.Context, CompletionRequest, func(string) error) (CompletionResult, *CompletionFailure) {
		return CompletionResult{}, &CompletionFailure{StatusCode: 503, Code: "no_capacity", Message: "No capacity.", RetryAfterSeconds: 2}
	}), testAuthenticator, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"cordova-chat-small-v1","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	request.Header.Set("Authorization", "Bearer test")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Content-Type") == "text/event-stream" || response.Header().Get("Retry-After") != "2" {
		t.Fatalf("response=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
}

func TestAccountedFailureAfterContentDoesNotEmitSuccessTerminator(t *testing.T) {
	handler, err := NewWithAuthAndExecutor(executorFunc(func(_ context.Context, request CompletionRequest, emit func(string) error) (CompletionResult, *CompletionFailure) {
		if err := request.Accepted(); err != nil {
			return CompletionResult{}, &CompletionFailure{StatusCode: 503, Code: "node_failed", Message: "Node failed."}
		}
		if err := emit("partial"); err != nil {
			return CompletionResult{}, &CompletionFailure{StatusCode: 503, Code: "node_failed", Message: "Node failed."}
		}
		return CompletionResult{}, &CompletionFailure{StatusCode: 503, Code: "node_failed", Message: "Node failed."}
	}), testAuthenticator, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"cordova-chat-small-v1","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	request.Header.Set("Authorization", "Bearer test")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `"code":"node_failed"`) || strings.Contains(body, "[DONE]") || strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("stream failure contract violated: status=%d body=%s", response.Code, body)
	}
}

var testAuthenticator Authenticator = func(r *http.Request) (Principal, bool) {
	if r.Header.Get("Authorization") != "Bearer test" {
		return Principal{}, false
	}
	return Principal{AccountID: "00000000-0000-4000-8000-000000000001", KeyID: "00000000-0000-4000-8000-000000000002",
		Kind: "key", Scopes: map[string]bool{"chat:write": true, "models:read": true}, AllowedModels: map[string]bool{ModelID: true}}, true
}
