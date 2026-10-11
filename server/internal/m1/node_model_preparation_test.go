package m1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestC3ModelPreparationRequiresCatalogModelAndSchedulers(t *testing.T) {
	router := &c3RequestRouter{nodes: &nodeService{}, models: map[string]NodeCatalogManifest{}}
	if _, err := router.PrepareNodeModel(context.Background(), "706134d8-0590-437c-ab24-bb9c396c815b", "unknown-model"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown model error=%v, want ErrNotFound", err)
	}
	router.models["cordova-chat-small-v1"] = NodeCatalogManifest{}
	if _, err := router.PrepareNodeModel(context.Background(), "706134d8-0590-437c-ab24-bb9c396c815b", "cordova-chat-small-v1"); !errors.Is(err, ErrNodeModelPreparationUnavailable) {
		t.Fatalf("missing schedulers error=%v, want ErrNodeModelPreparationUnavailable", err)
	}
}

func TestNodeModelPreparationErrorsUseSafeRetryResponses(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		status     int
		code       string
		retryAfter string
	}{
		{name: "model warming", err: ErrNodePreparationActive, status: http.StatusServiceUnavailable, code: "model_warming", retryAfter: "5"},
		{name: "no capacity", err: ErrNodeSessionUnavailable, status: http.StatusServiceUnavailable, code: "no_capacity", retryAfter: "2"},
		{name: "unknown model", err: ErrNotFound, status: http.StatusNotFound, code: "model_not_found"},
		{name: "invalid request", err: ErrInvalid, status: http.StatusBadRequest, code: "invalid_request"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			writeNodePreparationError(response, testCase.err)
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if response.Code != testCase.status || body.Error.Code != testCase.code ||
				response.Header().Get("Retry-After") != testCase.retryAfter {
				t.Fatalf("status=%d code=%q Retry-After=%q", response.Code, body.Error.Code, response.Header().Get("Retry-After"))
			}
		})
	}
}
