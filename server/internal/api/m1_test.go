package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cordova/server/internal/inference"
)

func TestAuthenticatedM1CompletionPersistsUsageBeforeSuccess(t *testing.T) {
	accountID := "11111111-1111-4111-8111-111111111111"
	keyID := "22222222-2222-4222-8222-222222222222"
	saved := false
	handler, err := NewWithAuth(inference.Mock{}, func(*http.Request) (Principal, bool) {
		return Principal{AccountID: accountID, KeyID: keyID, Kind: "key", Scopes: map[string]bool{"chat:write": true}}, true
	}, func(_ context.Context, account, key, request, model string, usage inference.Usage, latency time.Duration) error {
		if account != accountID || key != keyID || request == "" || model != ModelID || usage.TotalTokens != usage.PromptTokens+usage.CompletionTokens || latency < 0 {
			t.Errorf("invalid usage sink data: %q %q %q %q %+v %s", account, key, request, model, usage, latency)
		}
		saved = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !saved || !strings.Contains(recorder.Body.String(), "chat.completion") {
		t.Fatalf("completion failed or was not persisted: status=%d saved=%v body=%s", recorder.Code, saved, recorder.Body.String())
	}
}

func TestM1StorageFailureCannotReturnSuccessfulCompletion(t *testing.T) {
	handler, err := NewWithAuth(inference.Mock{}, func(*http.Request) (Principal, bool) {
		return Principal{AccountID: "11111111-1111-4111-8111-111111111111", Kind: "key", Scopes: map[string]bool{"chat:write": true}}, true
	}, func(context.Context, string, string, string, string, inference.Usage, time.Duration) error {
		return errors.New("storage failure")
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || strings.Contains(recorder.Body.String(), "chat.completion") {
		t.Fatalf("storage failure returned success: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestDashboardSessionCannotCreateChatCompletion(t *testing.T) {
	handler, err := NewWithAuth(inference.Mock{}, func(*http.Request) (Principal, bool) {
		return Principal{AccountID: "11111111-1111-4111-8111-111111111111", Kind: "session"}, true
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("dashboard session created API completion: status=%d", recorder.Code)
	}
}
