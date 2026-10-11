package tokenizer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"cordova/server/internal/inference"
)

func TestProcessRuntimeUsesTemplateAndExactTokenizeModes(t *testing.T) {
	template := "{{ messages }}"
	templateDigest := sha256.Sum256([]byte(template))
	model := testServiceModel()
	model.TemplateSHA256 = hex.EncodeToString(templateDigest[:])
	artifact, err := filepath.Abs(filepath.Join(t.TempDir(), "model.gguf"))
	if err != nil {
		t.Fatal(err)
	}

	var templateMessages []nativeMessage
	var tokenRequests []nativeTokenizeRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer native-test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/props":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_slots": 1, "model_path": artifact, "chat_template": template, "build_info": "extra fields are allowed",
			})
		case "/apply-template":
			var input nativeTemplateRequest
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode template request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			templateMessages = input.Messages
			_ = json.NewEncoder(w).Encode(nativeTemplateResponse{Prompt: stringPointer("templated prompt")})
		case "/tokenize":
			var input nativeTokenizeRequest
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Errorf("decode tokenize request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			tokenRequests = append(tokenRequests, input)
			tokens := []uint32{31, 32}
			if input.AddSpecial {
				tokens = []uint32{1, 31, 32}
			}
			_ = json.NewEncoder(w).Encode(nativeTokenizeResponse{Tokens: tokens})
		default:
			t.Errorf("unexpected llama-server endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	runner := &modelRunner{
		model: model, artifact: artifact, baseURL: server.URL, apiKey: "native-test-key", client: server.Client(),
	}
	runtime := &ProcessRuntime{models: map[string]*modelRunner{model.ID: runner}}
	ready, err := runtime.CheckReady(context.Background(), []Model{model})
	if err != nil || !reflect.DeepEqual(ready, []Model{model}) {
		t.Fatalf("CheckReady() = %v, %v", ready, err)
	}

	messages := []inference.Message{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "world"}}
	ids, err := runtime.PrepareInput(context.Background(), model, messages)
	if err != nil || !reflect.DeepEqual(ids, []uint32{1, 31, 32}) {
		t.Fatalf("PrepareInput() = %v, %v", ids, err)
	}
	if !reflect.DeepEqual(templateMessages, []nativeMessage{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "world"}}) {
		t.Fatalf("template messages = %#v", templateMessages)
	}

	count, err := runtime.CountOutput(context.Background(), model, "<|im_start|>plain output")
	if err != nil || count != 2 {
		t.Fatalf("CountOutput() = %d, %v", count, err)
	}
	if len(tokenRequests) != 2 {
		t.Fatalf("tokenizer request count = %d", len(tokenRequests))
	}
	if tokenRequests[0] != (nativeTokenizeRequest{Content: "templated prompt", AddSpecial: true, ParseSpecial: true, WithPieces: false}) {
		t.Fatalf("input tokenize request = %+v", tokenRequests[0])
	}
	if tokenRequests[1] != (nativeTokenizeRequest{Content: "<|im_start|>plain output", AddSpecial: false, ParseSpecial: false, WithPieces: false}) {
		t.Fatalf("output tokenize request = %+v", tokenRequests[1])
	}
}

func TestVerifyNativeArtifactRequiresExactSignedBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.gguf")
	contents := []byte("signed model bytes")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	wantHash := hex.EncodeToString(digest[:])
	if err := verifyNativeArtifact(context.Background(), path, wantHash, uint64(len(contents))); err != nil {
		t.Fatalf("valid artifact rejected: %v", err)
	}
	if err := verifyNativeArtifact(context.Background(), path, strings.Repeat("0", 64), uint64(len(contents))); err == nil {
		t.Fatal("artifact with mismatched digest was accepted")
	}
	if err := verifyNativeArtifact(context.Background(), path, wantHash, uint64(len(contents)+1)); err == nil {
		t.Fatal("artifact with mismatched size was accepted")
	}
}

func TestNativeRunnerCommitAndEnvironmentAreBounded(t *testing.T) {
	expected := "abcdef0123456789abcdef0123456789abcdef01"
	if !nativeCommitMatches(expected, "abcdef0123456789") || nativeCommitMatches(expected, "abcdef01") ||
		nativeCommitMatches(expected, strings.ToUpper(expected)) {
		t.Fatal("runner commit prefix validation is incorrect")
	}
	environment := nativeEnvironment([]string{
		"PATH=C:\\tools", "LLAMA_ARG_HOST=0.0.0.0", "LLAMA_API_KEY=unexpected", "HF_TOKEN=secret", "CUDA_VISIBLE_DEVICES=0",
	})
	if !reflect.DeepEqual(environment, []string{"PATH=C:\\tools", "CUDA_VISIBLE_DEVICES=0"}) {
		t.Fatalf("runner environment = %#v", environment)
	}
}

func stringPointer(value string) *string { return &value }
