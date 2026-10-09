package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cordova/server/internal/mocknode"
)

func main() {
	if err := run(); err != nil {
		log.Printf("mock node stopped: %v", err)
		os.Exit(1)
	}
}

func run() error {
	token := strings.TrimSpace(os.Getenv("CORDOVA_MOCK_NODE_TOKEN"))
	if tokenFile := strings.TrimSpace(os.Getenv("CORDOVA_MOCK_NODE_TOKEN_FILE")); tokenFile != "" {
		data, err := os.ReadFile(tokenFile)
		if err != nil {
			return errors.New("mock node token file is unavailable")
		}
		token = strings.TrimSpace(string(data))
	}
	certPath := strings.TrimSpace(os.Getenv("CORDOVA_MOCK_NODE_TLS_CERT"))
	keyPath := strings.TrimSpace(os.Getenv("CORDOVA_MOCK_NODE_TLS_KEY"))
	if token == "" || certPath == "" || keyPath == "" {
		return errors.New("mock node token and TLS certificate paths are required")
	}
	certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return errors.New("mock node TLS identity could not be loaded")
	}
	handler, err := mocknode.NewServer(token, 40*time.Millisecond)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: envOr("CORDOVA_MOCK_NODE_LISTEN", ":8090"), Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, TLSConfig: &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServeTLS("", "") }()
	select {
	case err := <-result:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("mock node listener failed")
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return errors.New("mock node shutdown failed")
		}
	}
	return nil
}

func envOr(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}
