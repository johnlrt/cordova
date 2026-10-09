package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"cordova/server/internal/api"
	"cordova/server/internal/inference"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	address := flag.String("listen", "127.0.0.1:8080", "HTTP listen address; put HTTPS in front before exposing it")
	keyPath := flag.String("key-file", ".local/api-key.txt", "path to the local demo API key")
	webDir := flag.String("web-dir", "", "optional frontend static build directory; never use the project root")
	initKey := flag.Bool("init-key", false, "create a random local key without overwriting an existing file, then exit")
	version := flag.Bool("version", false, "print the current implementation stage")
	flag.Parse()
	if *version {
		if m1Mode() {
			fmt.Println("Cordova M1 mock slice / test credits only / no real model or billing")
		} else {
			fmt.Println("Cordova Stage 1 mock API / synthetic word counts / no real model or billing")
		}
		return nil
	}
	if m1Mode() {
		return runM1(*address)
	}
	if *initKey {
		return createKey(*keyPath)
	}
	key := strings.TrimSpace(os.Getenv("CORDOVA_API_KEY"))
	if key == "" {
		contents, err := os.ReadFile(*keyPath)
		if err != nil {
			return fmt.Errorf("read demo key: run with -init-key first or set CORDOVA_API_KEY")
		}
		key = strings.TrimSpace(string(contents))
	}
	if len(key) > 4096 {
		return errors.New("demo key is too large")
	}
	handler, err := api.New(key, inference.Mock{Delay: 40 * time.Millisecond})
	if err != nil {
		return err
	}
	if *webDir != "" {
		handler, err = api.WithFrontend(handler, *webDir)
		if err != nil {
			return err
		}
	}
	server := api.HTTPServer(*address, handler)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	log.Printf("Cordova stage 1 listening on %s (MOCK responses; no model; no billing)", *address)
	select {
	case err := <-result:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP: %w", err)
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return fmt.Errorf("shutdown HTTP: %w", err)
		}
	}
	return nil
}

func m1Mode() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("CORDOVA_M1_MODE")), "true") || strings.TrimSpace(os.Getenv("CORDOVA_M1_MODE")) == "1"
}

func createKey(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create local key directory: %w", err)
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return fmt.Errorf("generate demo key: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create key file (existing keys are preserved): %w", err)
	}
	_, writeErr := fmt.Fprintln(file, "cvk_demo_"+hex.EncodeToString(secret[:]))
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write demo key: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close demo key: %w", closeErr)
	}
	fmt.Println("Created local demo key at " + path + ". Keep this file private.")
	return nil
}
