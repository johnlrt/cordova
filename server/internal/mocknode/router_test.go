package mocknode

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cordova/server/internal/api"
	"cordova/server/internal/domain"
	"cordova/server/internal/inference"
	requestcoord "cordova/server/internal/request"
)

const testOwnerID = "00000000-0000-4000-8000-000000000003"

func newTestRouter(t *testing.T, delay time.Duration, capacity int) *RequestRouter {
	t.Helper()
	token := strings.Repeat("r", 64)
	handler, err := NewServer(token, delay)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	provider, err := NewProvider("wss"+strings.TrimPrefix(server.URL, "https")+"/ws", token, roots)
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewRequestRouter(provider, testOwnerID, capacity)
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func TestRequestRouterBoundsLeasesAndHonorsDeadline(t *testing.T) {
	router := newTestRouter(t, 0, 1)
	deadline := time.Now().Add(time.Second)
	first, err := router.Acquire(context.Background(), api.ModelID, nil, deadline)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := router.Acquire(ctx, api.ModelID, nil, deadline); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second acquire error=%v, want bounded wait timeout", err)
	}
	first.Release(context.Background())
	second, err := router.Acquire(context.Background(), api.ModelID, nil, deadline)
	if err != nil {
		t.Fatalf("released capacity was not reusable: %v", err)
	}
	second.Release(context.Background())
	if _, err := router.Acquire(context.Background(), api.ModelID, map[string]struct{}{testOwnerID: {}}, deadline); !errors.Is(err, requestcoord.ErrNoCapacity) {
		t.Fatalf("excluded owner was selected: %v", err)
	}
}

func TestRequestRouterReconnectFencesOldAttempt(t *testing.T) {
	router := newTestRouter(t, 50*time.Millisecond, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	attempt, err := router.Acquire(ctx, api.ModelID, nil, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{}, 1)
	chunks := make(chan string, 8)
	finished := make(chan error, 1)
	go func() {
		_, runErr := attempt.(requestcoord.AcceptedAttempt).RunAccepted(ctx,
			inference.Request{Messages: []inference.Message{{Role: "user", Content: "hello"}}, MaxTokens: 8},
			func() error { accepted <- struct{}{}; return nil },
			func(text string) error { chunks <- text; return nil })
		finished <- runErr
	}()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("mock node did not acknowledge inference")
	}
	router.Disconnect()
	router.Reconnect()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("old session attempt completed after reconnect")
		}
	case <-time.After(time.Second):
		t.Fatal("fenced attempt did not stop")
	}
	select {
	case text := <-chunks:
		t.Fatalf("old session forwarded a chunk after fencing: %q", text)
	default:
	}
	attempt.Release(context.Background())
}

func TestRequestRouterRequiresValidOwnerID(t *testing.T) {
	provider := &Provider{}
	if _, err := NewRequestRouter(provider, "not-an-id", 1); err == nil {
		t.Fatal("invalid owner ID accepted")
	}
	if _, err := domain.ParseID(testOwnerID); err != nil {
		t.Fatal(err)
	}
}
