package api

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cordova/server/internal/inference"
)

func TestStreamFlushAndClientCancellation(t *testing.T) {
	cancelled := make(chan struct{})
	h := handler(t, providerFunc(func(ctx context.Context, req inference.Request, emit func(string) error) (inference.Result, error) {
		defer close(cancelled)
		if err := emit("first-content"); err != nil {
			return inference.Result{}, err
		}
		<-ctx.Done()
		return inference.Result{}, ctx.Err()
	}))
	server := httptest.NewServer(h)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := strings.TrimSuffix(validBody, "}") + `,"stream":true}`
	req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	var received strings.Builder
	for !strings.Contains(received.String(), "first-content") {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("content was not flushed while provider was still running: %v", err)
		}
		received.WriteString(line)
	}
	cancel()
	_ = response.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnected client did not cancel provider")
	}
}

func TestConcurrencyLimitReleasesCapacity(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	h := handler(t, providerFunc(func(ctx context.Context, req inference.Request, emit func(string) error) (inference.Result, error) {
		started <- struct{}{}
		select {
		case <-release:
			return inference.Result{FinishReason: "stop"}, nil
		case <-ctx.Done():
			return inference.Result{}, ctx.Err()
		}
	}))
	var jobs sync.WaitGroup
	for range 4 {
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			call(h, "POST", "/v1/chat/completions", validBody, testKey)
		}()
	}
	defer jobs.Wait()
	defer finish()
	for range 4 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("provider did not start")
		}
	}
	w := call(h, "POST", "/v1/chat/completions", validBody, testKey)
	if w.Code != 429 || w.Header().Get("Retry-After") != "2" {
		t.Fatal("fifth concurrent request was not rejected")
	}
	finish()
	jobs.Wait()
	if call(h, "POST", "/v1/chat/completions", validBody, testKey).Code != 200 {
		t.Fatal("completed requests did not release capacity")
	}
}

func TestRoutesAndMethods(t *testing.T) {
	h := handler(t, inference.Mock{})
	for _, tc := range []struct {
		method, path string
		want         int
	}{{"GET", "/", 404}, {"POST", "/v1/models", 405}, {"GET", "/v1/chat/completions", 405}} {
		w := call(h, tc.method, tc.path, "", testKey)
		if w.Code != tc.want {
			t.Fatalf("%s %s: %d", tc.method, tc.path, w.Code)
		}
	}
}

type failingWriter struct {
	http.ResponseWriter
}

func (f failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestWriteErrorPropagates(t *testing.T) {
	if err := writeEvent(failingWriter{httptest.NewRecorder()}, []byte("test")); err == nil {
		t.Fatal("write failure was swallowed")
	}
}
