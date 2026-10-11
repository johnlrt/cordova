package tokenizer

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cordova/server/internal/inference"
)

const serviceToken = "s12345678901234567890123456789012"

type serviceRuntime struct {
	mu          sync.Mutex
	readyModels []Model
	readyErr    error
	inputIDs    []uint32
	inputErr    error
	outputCount uint32
	outputErr   error
	messages    []inference.Message
	countedText string
	entered     chan struct{}
	release     chan struct{}
}

func (r *serviceRuntime) CheckReady(_ context.Context, models []Model) ([]Model, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.readyErr != nil {
		return nil, r.readyErr
	}
	if r.readyModels != nil {
		return append([]Model(nil), r.readyModels...), nil
	}
	return append([]Model(nil), models...), nil
}

func (r *serviceRuntime) PrepareInput(ctx context.Context, _ Model, messages []inference.Message) ([]uint32, error) {
	r.mu.Lock()
	r.messages = append([]inference.Message(nil), messages...)
	inputErr := r.inputErr
	ids := append([]uint32(nil), r.inputIDs...)
	entered, release := r.entered, r.release
	r.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return ids, inputErr
}

func (r *serviceRuntime) CountOutput(_ context.Context, _ Model, text string) (uint32, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.countedText = text
	return r.outputCount, r.outputErr
}

func TestServiceServesOnlyAuthenticatedCatalogBoundTokenization(t *testing.T) {
	model := testServiceModel()
	runtime := &serviceRuntime{inputIDs: []uint32{31, 41, 59}, outputCount: 2}
	service := newTestService(t, runtime)
	handler := service.Handler()

	unauthorized := serviceRequest(handler, http.MethodGet, "/readyz", "", nil)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized readiness status=%d", unauthorized.Code)
	}
	ready := serviceRequest(handler, http.MethodGet, "/readyz", serviceToken, nil)
	if ready.Code != http.StatusOK || !strings.Contains(ready.Body.String(), `"status":"ready"`) || !strings.Contains(ready.Body.String(), model.ID) {
		t.Fatalf("readiness response: status=%d body=%s", ready.Code, ready.Body.String())
	}

	prepareBody, err := json.Marshal(prepareServiceRequest{
		ModelID: model.ID, CatalogVersion: testCatalog,
		Messages: []serviceMessage{serviceMessageValue("system", "Be helpful."), serviceMessageValue("user", "é<|control|>")},
	})
	if err != nil {
		t.Fatal(err)
	}
	preparedResponse := serviceRequest(handler, http.MethodPost, "/internal/tokenizer/prepare", serviceToken, prepareBody)
	if preparedResponse.Code != http.StatusOK {
		t.Fatalf("prepare status=%d body=%s", preparedResponse.Code, preparedResponse.Body.String())
	}
	var prepared prepareResponse
	if err := json.Unmarshal(preparedResponse.Body.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	if prepared.InputTokens != 3 || len(prepared.InputTokenIDs) != 3 || prepared.ContextTokens != model.ContextTokens ||
		prepared.TokenizerSHA256 != model.TokenizerSHA256 || prepared.TemplateSHA256 != model.TemplateSHA256 {
		t.Fatalf("unexpected prepare response: %+v", prepared)
	}
	countedText := "é whole decoded output <|control|>"
	countBody, err := json.Marshal(countServiceRequest{ModelID: model.ID, CatalogVersion: testCatalog, Text: countedText})
	if err != nil {
		t.Fatal(err)
	}
	countResponse := serviceRequest(handler, http.MethodPost, "/internal/tokenizer/count", serviceToken, countBody)
	if countResponse.Code != http.StatusOK || !strings.Contains(countResponse.Body.String(), `"tokens":2`) {
		t.Fatalf("count response: status=%d body=%s", countResponse.Code, countResponse.Body.String())
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.countedText != countedText || len(runtime.messages) != 2 || runtime.messages[1].Content != "é<|control|>" {
		t.Fatal("service changed prompt or output text before native tokenization")
	}
}

func TestServiceRejectsMalformedRequestsAndMapsSafeErrors(t *testing.T) {
	runtime := &serviceRuntime{inputIDs: []uint32{7}}
	handler := newTestService(t, runtime).Handler()
	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{name: "unknown field", body: `{"model_id":"` + testModelID + `","catalog_version":"` + testCatalog + `","messages":[{"role":"user","content":"x"}],"prompt":"hidden"}`, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "duplicate key", body: `{"model_id":"` + testModelID + `","model_id":"` + testModelID + `","catalog_version":"` + testCatalog + `","messages":[{"role":"user","content":"x"}]}`, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "null model", body: `{"model_id":null,"catalog_version":"` + testCatalog + `","messages":[{"role":"user","content":"x"}]}`, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "missing message content", body: `{"model_id":"` + testModelID + `","catalog_version":"` + testCatalog + `","messages":[{"role":"user"}]}`, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "null message content", body: `{"model_id":"` + testModelID + `","catalog_version":"` + testCatalog + `","messages":[{"role":"user","content":null}]}`, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "invalid model slug", body: `{"model_id":"bad--model","catalog_version":"` + testCatalog + `","messages":[{"role":"user","content":"x"}]}`, status: http.StatusBadRequest, code: "invalid_request"},
		{name: "unknown model", body: `{"model_id":"other-model","catalog_version":"` + testCatalog + `","messages":[{"role":"user","content":"x"}]}`, status: http.StatusNotFound, code: "not_found"},
		{name: "catalog mismatch", body: `{"model_id":"` + testModelID + `","catalog_version":"00000000-0000-4000-8000-000000000099","messages":[{"role":"user","content":"x"}]}`, status: http.StatusConflict, code: "conflict"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			response := serviceRequest(handler, http.MethodPost, "/internal/tokenizer/prepare", serviceToken, []byte(test.body))
			if response.Code != test.status || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
			}
			if serviceErrorCode(t, response) != test.code {
				t.Fatalf("error code did not match %q", test.code)
			}
		})
	}

	missingAuth := serviceRequest(handler, http.MethodPost, "/internal/tokenizer/prepare", "", []byte(`{"model_id":"`+testModelID+`","catalog_version":"`+testCatalog+`","messages":[{"role":"user","content":"x"}]}`))
	if missingAuth.Code != http.StatusUnauthorized || serviceErrorCode(t, missingAuth) != "unauthenticated" {
		t.Fatalf("missing helper auth response: %d %s", missingAuth.Code, missingAuth.Body.String())
	}
}

func TestServiceFailsClosedForUnavailableRuntimeAndContextOverflow(t *testing.T) {
	model := testServiceModel()
	requestBody, err := json.Marshal(prepareServiceRequest{
		ModelID: model.ID, CatalogVersion: testCatalog, Messages: []serviceMessage{serviceMessageValue("user", "hello")},
	})
	if err != nil {
		t.Fatal(err)
	}
	noRuntime := newTestService(t, nil).Handler()
	unavailable := serviceRequest(noRuntime, http.MethodPost, "/internal/tokenizer/prepare", serviceToken, requestBody)
	if unavailable.Code != http.StatusServiceUnavailable || serviceErrorCode(t, unavailable) != "tokenizer_unavailable" {
		t.Fatalf("nil runtime response: %d %s", unavailable.Code, unavailable.Body.String())
	}

	runtime := &serviceRuntime{inputErr: inference.ErrContextExceeded}
	contextService := newTestService(t, runtime).Handler()
	contextExceeded := serviceRequest(contextService, http.MethodPost, "/internal/tokenizer/prepare", serviceToken, requestBody)
	if contextExceeded.Code != http.StatusBadRequest || serviceErrorCode(t, contextExceeded) != "context_exceeded" {
		t.Fatalf("context response: %d %s", contextExceeded.Code, contextExceeded.Body.String())
	}

	tooManyIDs := newTestService(t, &serviceRuntime{inputIDs: make([]uint32, maxPreparedInputTokens+1)}).Handler()
	invalidRuntimeOutput := serviceRequest(tooManyIDs, http.MethodPost, "/internal/tokenizer/prepare", serviceToken, requestBody)
	if invalidRuntimeOutput.Code != http.StatusServiceUnavailable || serviceErrorCode(t, invalidRuntimeOutput) != "tokenizer_unavailable" {
		t.Fatalf("invalid runtime output response: %d %s", invalidRuntimeOutput.Code, invalidRuntimeOutput.Body.String())
	}
}

func TestServiceBoundsPerModelConcurrencyAndQueueWait(t *testing.T) {
	model := testServiceModel()
	runtime := &serviceRuntime{
		inputIDs: []uint32{7}, entered: make(chan struct{}, 3), release: make(chan struct{}),
	}
	handler := newTestService(t, runtime).Handler()
	body, err := json.Marshal(prepareServiceRequest{
		ModelID: model.ID, CatalogVersion: testCatalog, Messages: []serviceMessage{serviceMessageValue("user", "hello")},
	})
	if err != nil {
		t.Fatal(err)
	}
	responses := make(chan *httptest.ResponseRecorder, 3)
	for range 2 {
		go func() {
			responses <- serviceRequest(handler, http.MethodPost, "/internal/tokenizer/prepare", serviceToken, body)
		}()
	}
	for range 2 {
		select {
		case <-runtime.entered:
		case <-time.After(time.Second):
			t.Fatal("first two calls did not enter the runtime")
		}
	}
	go func() {
		responses <- serviceRequest(handler, http.MethodPost, "/internal/tokenizer/prepare", serviceToken, body)
	}()
	select {
	case response := <-responses:
		if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" || serviceErrorCode(t, response) != "rate_limited" {
			t.Fatalf("queued call response: %d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("queued call did not respect its 500ms queue wait")
	}
	close(runtime.release)
	for range 2 {
		select {
		case response := <-responses:
			if response.Code != http.StatusOK {
				t.Fatalf("active call status=%d body=%s", response.Code, response.Body.String())
			}
		case <-time.After(time.Second):
			t.Fatal("active tokenizer call did not finish after runtime was released")
		}
	}
}

func testServiceModel() Model {
	return Model{
		ID: testModelID, CatalogVersion: testCatalog, ContextTokens: 64, AddBOS: true,
		ArtifactURL: "https://models.example.test/cordova-chat-small-v1.gguf", ArtifactSHA256: strings.Repeat("c", 64),
		ArtifactSizeBytes: 1024, LlamaCommit: strings.Repeat("d", 40),
		TokenizerSHA256: testTokenizerSHA, TemplateSHA256: testTemplateSHA,
	}
}

func TestNewServiceRequiresCompleteModelArtifactIdentity(t *testing.T) {
	model := testServiceModel()
	cases := []Model{
		func() Model { value := model; value.ArtifactSHA256 = ""; return value }(),
		func() Model {
			value := model
			value.ArtifactURL = "http://models.example.test/model.gguf"
			return value
		}(),
		func() Model { value := model; value.LlamaCommit = "abcd"; return value }(),
		func() Model { value := model; value.ArtifactSizeBytes = 0; return value }(),
	}
	for i, invalid := range cases {
		if _, err := NewService(testCatalog, serviceToken, []Model{invalid}, nil); err == nil {
			t.Fatalf("invalid model artifact identity %d was accepted", i)
		}
	}
}

func serviceMessageValue(role, content string) serviceMessage {
	return serviceMessage{Role: &role, Content: &content}
}

func newTestService(t *testing.T, runtime Runtime) *Service {
	t.Helper()
	service, err := NewService(testCatalog, serviceToken, []Model{testServiceModel()}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func serviceRequest(handler http.Handler, method, path, token string, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func serviceErrorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode service error: %v", err)
	}
	if !validUUID(payload.RequestID) {
		t.Fatalf("service error request_id=%q is invalid", payload.RequestID)
	}
	return payload.Error.Code
}
