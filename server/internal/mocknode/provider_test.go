package mocknode

import (
	"context"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cordova/server/internal/inference"
	"github.com/gorilla/websocket"
)

func TestWebSocketProviderStreamsAndRecountsSyntheticUsage(t *testing.T) {
	token := strings.Repeat("s", 64)
	handler, err := NewServer(token, 0)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	provider, err := NewProvider("wss"+strings.TrimPrefix(server.URL, "https")+"/ws", token, roots)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	result, err := provider.Generate(context.Background(), inference.Request{Messages: []inference.Message{{Role: "user", Content: "hello world"}}, MaxTokens: 64}, func(chunk string) error { output.WriteString(chunk); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(output.String(), "[MOCK]") || result.FinishReason != "stop" {
		t.Fatalf("unexpected mock completion %q / %q", output.String(), result.FinishReason)
	}
	if result.Usage.PromptTokens != 2 || result.Usage.CompletionTokens != len(strings.Fields(output.String())) || result.Usage.TotalTokens != result.Usage.PromptTokens+result.Usage.CompletionTokens {
		t.Fatalf("unexpected synthetic usage: %+v", result.Usage)
	}
}

func TestWebSocketProviderRejectsInvalidConfigurationAndFrames(t *testing.T) {
	if _, err := NewProvider("ws://localhost/ws", strings.Repeat("x", 64), nil); err == nil {
		t.Fatal("insecure ws endpoint accepted")
	}
	if _, err := NewProvider("wss://localhost/ws", "short", nil); err == nil {
		t.Fatal("short mock-node token accepted")
	}
	token := strings.Repeat("t", 64)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, err = conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"v":1,"type":"delta","type":"complete"}`))
	}))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	provider, err := NewProvider("wss"+strings.TrimPrefix(server.URL, "https"), token, roots)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = provider.Generate(ctx, inference.Request{Messages: []inference.Message{{Role: "user", Content: "hello"}}, MaxTokens: 2}, func(string) error { return nil })
	if err == nil {
		t.Fatal("duplicate-key mock-node frame was accepted")
	}
}
