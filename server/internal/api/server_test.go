package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cordova/server/internal/inference"
)

const testKey = "cvk_demo_this-is-a-public-test-fixture-only"
const validBody = `{"model":"cordova-chat-small-v1","messages":[{"role":"user","content":"Hello"}]}`

func handler(t *testing.T, provider inference.Provider) http.Handler {
	t.Helper()
	h, err := New(testKey, provider)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func call(h http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestKeyAndModelCatalog(t *testing.T) {
	h := handler(t, inference.Mock{})
	for _, key := range []string{"", "incorrect"} {
		w := call(h, "GET", "/v1/models", "", key)
		if w.Code != 401 || !strings.Contains(w.Body.String(), "unauthenticated") {
			t.Fatalf("unauthorized call: %d %s", w.Code, w.Body.String())
		}
	}
	w := call(h, "GET", "/v1/models", "", testKey)
	if w.Code != 200 || w.Header().Get("X-Cordova-Mode") != "mock" || !strings.Contains(w.Body.String(), ModelID) {
		t.Fatalf("catalog: %d %s", w.Code, w.Body.String())
	}
	if len(w.Header().Get("X-Request-ID")) != 36 {
		t.Fatal("missing UUID request identifier")
	}
	if call(h, "GET", "/healthz", "", "").Code != 200 {
		t.Fatal("health must be available without the demo key")
	}
}

func TestInvalidRequests(t *testing.T) {
	h := handler(t, inference.Mock{})
	cases := []struct {
		name, body string
		status     int
	}{
		{"malformed", `{`, 400},
		{"null", `null`, 400},
		{"no model", `{"messages":[{"role":"user","content":"hi"}]}`, 400},
		{"unknown model", strings.Replace(validBody, ModelID, "unknown", 1), 404},
		{"unknown option", strings.TrimSuffix(validBody, "}") + `,"tools":[]}`, 400},
		{"duplicate model", strings.TrimSuffix(validBody, "}") + `,"model":"other"}`, 400},
		{"duplicate content", strings.Replace(validBody, `"content":"Hello"`, `"content":"Hello","content":"different"`, 1), 400},
		{"two objects", validBody + validBody, 400},
		{"negative max", strings.TrimSuffix(validBody, "}") + `,"max_tokens":-1}`, 400},
		{"too large max", strings.TrimSuffix(validBody, "}") + `,"max_tokens":2049}`, 400},
		{"fractional max", strings.TrimSuffix(validBody, "}") + `,"max_tokens":1.5}`, 400},
		{"null stream", strings.TrimSuffix(validBody, "}") + `,"stream":null}`, 400},
		{"missing content", strings.Replace(validBody, `,"content":"Hello"`, "", 1), 400},
		{"null content", strings.Replace(validBody, `"Hello"`, "null", 1), 400},
		{"array content", strings.Replace(validBody, `"Hello"`, "[]", 1), 400},
		{"tool role", strings.Replace(validBody, `"user"`, `"tool"`, 1), 400},
		{"no user turn", strings.Replace(validBody, `"user"`, `"system"`, 1), 400},
		{"options without streaming", strings.TrimSuffix(validBody, "}") + `,"stream_options":{"include_usage":true}}`, 400},
		{"null usage", strings.TrimSuffix(validBody, "}") + `,"stream":true,"stream_options":{"include_usage":null}}`, 400},
		{"large body", strings.Replace(validBody, "Hello", strings.Repeat("x", 1<<20), 1), 400},
		{"large message", strings.Replace(validBody, "Hello", strings.Repeat("x", 256<<10+1), 1), 400},
		{"invalid utf8", strings.Replace(validBody, "Hello", string([]byte{0xff}), 1), 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := call(h, "POST", "/v1/chat/completions", tc.body, testKey)
			if w.Code != tc.status {
				t.Fatalf("want %d got %d: %s", tc.status, w.Code, w.Body.String())
			}
			var response errorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Error.Code == "" || response.RequestID == "" {
				t.Fatal("error must have the documented shape")
			}
		})
	}
}

func TestBufferedAndStreamingResponses(t *testing.T) {
	h := handler(t, inference.Mock{})
	w := call(h, "POST", "/v1/chat/completions", validBody, testKey)
	var full struct {
		ID      string `json:"id"`
		Choices []struct {
			Message      inference.Message `json:"message"`
			FinishReason string            `json:"finish_reason"`
		} `json:"choices"`
		Usage inference.Usage `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &full); err != nil || w.Code != 200 || len(full.Choices) != 1 {
		t.Fatalf("buffered: %d %s", w.Code, w.Body.String())
	}
	body := strings.TrimSuffix(validBody, "}") + `,"stream":true,"stream_options":{"include_usage":true}}`
	sse := call(h, "POST", "/v1/chat/completions", body, testKey)
	if sse.Header().Get("Content-Type") != "text/event-stream" || !strings.HasSuffix(sse.Body.String(), "data: [DONE]\n\n") {
		t.Fatal("SSE must end successfully with DONE")
	}
	var text, id string
	usageChunks, finishes, roles := 0, 0, 0
	for _, line := range strings.Split(sse.Body.String(), "\n") {
		if line == "" || line == "data: [DONE]" {
			continue
		}
		var part chunk
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &part); err != nil {
			t.Fatal(err)
		}
		if id == "" {
			id = part.ID
		}
		if part.ID != id || part.Object != "chat.completion.chunk" || part.Model != ModelID {
			t.Fatal("inconsistent chunk identity")
		}
		for _, choice := range part.Choices {
			text += choice.Delta.Content
			if choice.Delta.Role == "assistant" {
				roles++
			}
			if choice.FinishReason != nil {
				finishes++
			}
		}
		if part.Usage != nil {
			usageChunks++
			if len(part.Choices) != 0 || *part.Usage != full.Usage {
				t.Fatal("invalid final synthetic usage chunk")
			}
		}
	}
	if text != full.Choices[0].Message.Content || !strings.HasPrefix(text, "[MOCK]") || usageChunks != 1 || finishes != 1 || roles != 1 {
		t.Fatal("stream and buffered response differ or terminal chunks are invalid")
	}
}

func TestC2GenerationParametersReachProvider(t *testing.T) {
	var captured inference.Request
	provider := providerFunc(func(ctx context.Context, req inference.Request, emit func(string) error) (inference.Result, error) {
		captured = req
		if err := emit("synthetic"); err != nil {
			return inference.Result{}, err
		}
		return inference.Result{FinishReason: "stop"}, nil
	})
	body := strings.TrimSuffix(validBody, "}") + `,"temperature":0,"top_p":0.5,"stop":["END","готово"],"seed":123,"n":1,"user":"discarded"}`
	w := call(handler(t, provider), "POST", "/v1/chat/completions", body, testKey)
	if w.Code != 200 {
		t.Fatalf("completion: %d %s", w.Code, w.Body.String())
	}
	if captured.MaxTokens != 512 || captured.Parameters.Temperature != 0 || captured.Parameters.TopP != 0.5 || captured.Parameters.Seed == nil || *captured.Parameters.Seed != 123 {
		t.Fatalf("unexpected provider request: %#v", captured)
	}
	if len(captured.Parameters.Stop) != 2 || captured.Parameters.Stop[0] != "END" || captured.Parameters.Stop[1] != "готово" {
		t.Fatalf("stop values not normalized: %#v", captured.Parameters.Stop)
	}
	if strings.Contains(w.Body.String(), "discarded") {
		t.Fatal("user metadata leaked into the response")
	}
}

func TestInvalidProviderUsageCannotReturnSuccess(t *testing.T) {
	provider := providerFunc(func(context.Context, inference.Request, func(string) error) (inference.Result, error) {
		return inference.Result{FinishReason: "stop", Usage: inference.Usage{PromptTokens: -1, CompletionTokens: 2, TotalTokens: 1}}, nil
	})
	w := call(handler(t, provider), "POST", "/v1/chat/completions", validBody, testKey)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), `"object":"chat.completion"`) {
		t.Fatalf("invalid usage metadata returned success: %d %s", w.Code, w.Body.String())
	}
}

func TestC2GenerationDefaultsAndStopString(t *testing.T) {
	var captured inference.Request
	provider := providerFunc(func(ctx context.Context, req inference.Request, emit func(string) error) (inference.Result, error) {
		captured = req
		return inference.Result{FinishReason: "stop"}, nil
	})
	body := strings.TrimSuffix(validBody, "}") + `,"stop":"END"}`
	w := call(handler(t, provider), "POST", "/v1/chat/completions", body, testKey)
	if w.Code != 200 {
		t.Fatalf("completion: %d %s", w.Code, w.Body.String())
	}
	if captured.Parameters.Temperature != 0.7 || captured.Parameters.TopP != 0.95 || captured.Parameters.Seed != nil {
		t.Fatalf("unexpected parameter defaults: %#v", captured.Parameters)
	}
	if len(captured.Parameters.Stop) != 1 || captured.Parameters.Stop[0] != "END" {
		t.Fatalf("scalar stop was not normalized: %#v", captured.Parameters.Stop)
	}
}

func TestC2GenerationParameterValidation(t *testing.T) {
	base := strings.TrimSuffix(validBody, "}")
	tooLongStop := strings.Repeat("界", 129)
	cases := []struct {
		name, field, value, wantCode string
	}{
		{"temperature below minimum", "temperature", "-0.01", "invalid_request"},
		{"temperature above maximum", "temperature", "2.01", "invalid_request"},
		{"top_p zero", "top_p", "0", "invalid_request"},
		{"top_p above maximum", "top_p", "1.01", "invalid_request"},
		{"too many stop strings", "stop", `["a","b","c","d","e"]`, "invalid_request"},
		{"long stop string", "stop", `"` + tooLongStop + `"`, "invalid_request"},
		{"stop object", "stop", `{}`, "invalid_request"},
		{"stop null entry", "stop", `["a",null]`, "invalid_request"},
		{"negative seed", "seed", "-1", "invalid_request"},
		{"large seed", "seed", "2147483648", "invalid_request"},
		{"fractional seed", "seed", "1.5", "invalid_request"},
		{"n zero", "n", "0", "unsupported_parameter"},
		{"n two", "n", "2", "unsupported_parameter"},
		{"user wrong type", "user", "[]", "invalid_request"},
		{"temperature null", "temperature", "null", "invalid_request"},
		{"top_p null", "top_p", "null", "invalid_request"},
		{"stop null", "stop", "null", "invalid_request"},
		{"seed null", "seed", "null", "invalid_request"},
		{"n null", "n", "null", "invalid_request"},
		{"user null", "user", "null", "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := base + `,"` + tc.field + `":` + tc.value + `}`
			w := call(handler(t, inference.Mock{}), "POST", "/v1/chat/completions", body, testKey)
			if w.Code != 400 {
				t.Fatalf("want 400 got %d: %s", w.Code, w.Body.String())
			}
			var response errorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Error.Code != tc.wantCode {
				t.Fatalf("want code %q, got %s", tc.wantCode, w.Body.String())
			}
		})
	}
}

type providerFunc func(context.Context, inference.Request, func(string) error) (inference.Result, error)

func (f providerFunc) Generate(ctx context.Context, req inference.Request, emit func(string) error) (inference.Result, error) {
	return f(ctx, req, emit)
}

func TestFailureAfterContentDoesNotSendSuccess(t *testing.T) {
	h := handler(t, providerFunc(func(ctx context.Context, req inference.Request, emit func(string) error) (inference.Result, error) {
		if err := emit("partial"); err != nil {
			return inference.Result{}, err
		}
		return inference.Result{}, errors.New("private native error with sensitive details")
	}))
	body := strings.TrimSuffix(validBody, "}") + `,"stream":true}`
	w := call(h, "POST", "/v1/chat/completions", body, testKey)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "node_failed") || strings.Contains(w.Body.String(), "[DONE]") || strings.Contains(w.Body.String(), "private native") {
		t.Fatalf("unsafe stream failure: %d %s", w.Code, w.Body.String())
	}
}

func TestUnsupportedIdempotencyAndContentType(t *testing.T) {
	h := handler(t, inference.Mock{})
	for _, header := range []string{"Idempotency-Key", "Content-Type"} {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(validBody))
		r.Header.Set("Authorization", "Bearer "+testKey)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(header, "unsupported")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("unsupported %s accepted", header)
		}
	}
}

func TestSyntheticOutputLimit(t *testing.T) {
	body := strings.TrimSuffix(validBody, "}") + `,"max_tokens":1}`
	w := call(handler(t, inference.Mock{}), "POST", "/v1/chat/completions", body, testKey)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"finish_reason":"length"`) || !strings.Contains(w.Body.String(), `"completion_tokens":1`) {
		t.Fatal("synthetic output limit not applied")
	}
}

func TestNoKeyInErrors(t *testing.T) {
	h := handler(t, inference.Mock{})
	w := call(h, "POST", "/v1/chat/completions", `{"SECRET_SENTINEL":"private"}`, testKey)
	if strings.Contains(w.Body.String(), "SECRET_SENTINEL") || strings.Contains(w.Body.String(), testKey) {
		t.Fatal("rejected input or credential leaked")
	}
	if _, err := New("short", inference.Mock{}); err == nil {
		t.Fatal("weak startup key accepted")
	}
}
