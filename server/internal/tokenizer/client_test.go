package tokenizer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cordova/server/internal/inference"
)

const (
	testModelID      = "cordova-chat-small-v1"
	testCatalog      = "00000000-0000-4000-8000-000000000001"
	testTokenizerSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testTemplateSHA  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestClientPreparesCanonicalIDsAndCountsWholeText(t *testing.T) {
	var prepareRequestBody prepareRequest
	var countRequestBody countRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("s", 32) {
			t.Errorf("unexpected authorization header")
		}
		if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" {
			t.Errorf("unexpected content headers: %v", r.Header)
		}
		switch r.URL.Path {
		case "/internal/tokenizer/prepare":
			if err := json.NewDecoder(r.Body).Decode(&prepareRequestBody); err != nil {
				t.Errorf("decode prepare request: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fmt.Sprintf(`{"input_token_ids":[31,41,59],"input_tokens":3,"context_tokens":64,"tokenizer_sha256":%q,"template_sha256":%q}`, testTokenizerSHA, testTemplateSHA)))
		case "/internal/tokenizer/count":
			if err := json.NewDecoder(r.Body).Decode(&countRequestBody); err != nil {
				t.Errorf("decode count request: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tokens":2}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	messages := []inference.Message{{Role: "system", Content: "You are helpful."}, {Role: "user", Content: "hi"}}
	prepared, err := client.PrepareInput(context.Background(), testModelID, messages)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.TokenCount != 3 || prepared.ContextTokens != 64 || prepared.CatalogVersion != testCatalog ||
		len(prepared.TokenIDs) != 3 || prepared.TokenIDs[0] != 31 || prepared.TokenIDs[2] != 59 {
		t.Fatalf("prepared input=%+v", prepared)
	}
	if prepareRequestBody.ModelID != testModelID || prepareRequestBody.CatalogVersion != testCatalog || len(prepareRequestBody.Messages) != 2 {
		t.Fatalf("prepare request=%+v", prepareRequestBody)
	}
	text := "éabc<|control|>more"
	count, err := client.CountOutput(context.Background(), testModelID, text)
	if err != nil || count != 2 {
		t.Fatalf("output count=%d err=%v", count, err)
	}
	if countRequestBody.ModelID != testModelID || countRequestBody.CatalogVersion != testCatalog || countRequestBody.Text != text {
		t.Fatalf("count request did not preserve full text: %+v", countRequestBody)
	}
}

func TestClientFailsClosedForMismatchedOrMalformedPrepareResponses(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "hash mismatch", body: `{"input_token_ids":[7],"input_tokens":1,"context_tokens":64,"tokenizer_sha256":"` + strings.Repeat("c", 64) + `","template_sha256":"` + testTemplateSHA + `"}`},
		{name: "token count mismatch", body: `{"input_token_ids":[7,8],"input_tokens":1,"context_tokens":64,"tokenizer_sha256":"` + testTokenizerSHA + `","template_sha256":"` + testTemplateSHA + `"}`},
		{name: "unknown field", body: `{"input_token_ids":[7],"input_tokens":1,"context_tokens":64,"tokenizer_sha256":"` + testTokenizerSHA + `","template_sha256":"` + testTemplateSHA + `","prompt":"secret"}`},
		{name: "duplicate key", body: `{"input_token_ids":[7],"input_tokens":1,"input_tokens":1,"context_tokens":64,"tokenizer_sha256":"` + testTokenizerSHA + `","template_sha256":"` + testTemplateSHA + `"}`},
		{name: "missing required field", body: `{"input_token_ids":[7],"input_tokens":1,"context_tokens":64,"tokenizer_sha256":"` + testTokenizerSHA + `"}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client := newTestClient(t, server.URL)
			if _, err := client.PrepareInput(context.Background(), testModelID, []inference.Message{{Role: "user", Content: "hello"}}); !errors.Is(err, ErrCatalogMismatch) && !errors.Is(err, ErrUnavailable) {
				t.Fatalf("malformed response error=%v", err)
			}
		})
	}
}

func TestClientMapsUnavailableAndRejectsOversizeTextBeforeNetwork(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	if _, err := client.CountOutput(context.Background(), testModelID, "hello"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("service outage error=%v", err)
	}
	if _, err := client.CountOutput(context.Background(), testModelID, strings.Repeat("x", maxCountTextBytes+1)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("oversize text error=%v", err)
	}
	if requests != 1 {
		t.Fatalf("invalid text reached tokenizer service: requests=%d", requests)
	}
}

func TestClientCheckReadyRequiresExactCatalogModels(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{name: "ready", body: `{"status":"ready","catalog_version":"` + testCatalog + `","models":["` + testModelID + `"]}`},
		{name: "not ready", body: `{"status":"not_ready","catalog_version":null,"models":[]}`, want: ErrUnavailable},
		{name: "catalog mismatch", body: `{"status":"ready","catalog_version":"00000000-0000-4000-8000-000000000099","models":["` + testModelID + `"]}`, want: ErrCatalogMismatch},
		{name: "model mismatch", body: `{"status":"ready","catalog_version":"` + testCatalog + `","models":["other-model"]}`, want: ErrCatalogMismatch},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/readyz" || r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+strings.Repeat("s", 32) {
					t.Errorf("unexpected readiness request: method=%s path=%s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client := newTestClient(t, server.URL)
			err := client.CheckReady(context.Background())
			if test.want == nil && err != nil {
				t.Fatalf("ready catalog rejected: %v", err)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("readiness error=%v, want %v", err, test.want)
			}
		})
	}
}

func TestClientPreservesHelperContextExceededError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"context exceeded","type":"invalid_request_error","param":null,"code":"context_exceeded"},"request_id":"00000000-0000-4000-8000-000000000002"}`))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL)
	_, err := client.PrepareInput(context.Background(), testModelID, []inference.Message{{Role: "user", Content: "hello"}})
	if !errors.Is(err, inference.ErrContextExceeded) {
		t.Fatalf("context error=%v", err)
	}
}

func newTestClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	client, err := NewClient(endpoint, strings.Repeat("s", 32), []Model{{
		ID: testModelID, CatalogVersion: testCatalog, ContextTokens: 64,
		TokenizerSHA256: testTokenizerSHA, TemplateSHA256: testTemplateSHA,
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return client
}
