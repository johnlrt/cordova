package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cordova/server/internal/m1"
	"cordova/server/internal/tokenizer"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	listen := envOr("CORDOVA_TOKENIZER_LISTEN", "127.0.0.1:9001")
	modelDir := strings.TrimSpace(os.Getenv("CORDOVA_TOKENIZER_MODEL_DIR"))
	binary := strings.TrimSpace(os.Getenv("CORDOVA_LLAMA_SERVER"))
	if modelDir == "" {
		return errors.New("CORDOVA_TOKENIZER_MODEL_DIR is required")
	}
	catalogPath := strings.TrimSpace(os.Getenv("CORDOVA_MODEL_CATALOG_FILE"))
	if catalogPath == "" {
		return errors.New("CORDOVA_MODEL_CATALOG_FILE is required")
	}
	catalogData, err := readBoundedFile(catalogPath, m1.MaxNodeCatalogBytes)
	if err != nil {
		return errors.New("signed model catalog could not be read")
	}
	publicKeyText, err := configuredValue("CORDOVA_MODEL_CATALOG_PUBLIC_KEY", "CORDOVA_MODEL_CATALOG_PUBLIC_KEY_FILE", false)
	if err != nil {
		return errors.New("catalog verification key is not configured")
	}
	publicKey, err := decodePublicKey(publicKeyText)
	if err != nil {
		return errors.New("catalog verification key is invalid")
	}
	catalog, err := m1.ParseNodeCatalog(catalogData, publicKey)
	if err != nil || len(catalog.Models) == 0 {
		return errors.New("signed model catalog is invalid or empty")
	}

	helperToken, err := configuredValue("CORDOVA_TOKENIZER_TOKEN", "CORDOVA_TOKENIZER_TOKEN_FILE", true)
	if err != nil {
		return errors.New("tokenizer helper credential is not configured")
	}
	models := make([]tokenizer.Model, 0, len(catalog.Models))
	for _, manifest := range catalog.Models {
		models = append(models, tokenizer.Model{
			ID: manifest.Model.ID, CatalogVersion: catalog.Version,
			ContextTokens: manifest.Model.ContextTokens, AddBOS: manifest.AddBOS,
			ArtifactURL: manifest.ArtifactURL, ArtifactSHA256: manifest.ArtifactSHA256,
			ArtifactSizeBytes: manifest.ArtifactSizeBytes, LlamaCommit: manifest.LlamaCommit,
			TokenizerSHA256: manifest.TokenizerSHA256, TemplateSHA256: manifest.TemplateSHA256,
		})
	}

	startupTimeout := 2 * time.Minute
	if configured := strings.TrimSpace(os.Getenv("CORDOVA_TOKENIZER_STARTUP_TIMEOUT")); configured != "" {
		startupTimeout, err = time.ParseDuration(configured)
		if err != nil {
			return errors.New("tokenizer startup timeout is invalid")
		}
	}

	loopback, err := isLoopbackAddress(listen)
	if err != nil {
		return errors.New("tokenizer listen address is invalid")
	}
	certFile := strings.TrimSpace(os.Getenv("CORDOVA_TOKENIZER_TLS_CERT_FILE"))
	keyFile := strings.TrimSpace(os.Getenv("CORDOVA_TOKENIZER_TLS_KEY_FILE"))
	if (certFile == "") != (keyFile == "") || (!loopback && certFile == "") {
		return errors.New("non-loopback tokenizer listeners require a TLS certificate and key")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	native, err := tokenizer.NewProcessRuntime(ctx, tokenizer.ProcessConfig{
		BinaryPath: binary, ModelDir: modelDir,
		TempDir:        strings.TrimSpace(os.Getenv("CORDOVA_TOKENIZER_TEMP_DIR")),
		StartupTimeout: startupTimeout,
	}, models)
	if err != nil {
		return fmt.Errorf("tokenizer native runtime failed: %w", err)
	}
	defer native.Close()

	service, err := tokenizer.NewService(catalog.Version, helperToken, models, native)
	if err != nil {
		return errors.New("tokenizer service configuration is invalid")
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return errors.New("tokenizer listener could not start")
	}
	server := &http.Server{
		Handler:           service.Handler(),
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       3 * time.Second,
		WriteTimeout:      3 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	if certFile != "" {
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	serveResult := make(chan error, 1)
	go func() {
		if certFile == "" {
			serveResult <- server.Serve(listener)
			return
		}
		serveResult <- server.ServeTLS(listener, certFile, keyFile)
	}()
	log.Print("trusted tokenizer helper listening; native models ready")
	select {
	case err := <-serveResult:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("tokenizer listener stopped unexpectedly")
		}
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			_ = server.Close()
			return errors.New("tokenizer listener shutdown failed")
		}
	}
	return nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func configuredValue(valueName, fileName string, required bool) (string, error) {
	value := strings.TrimSpace(os.Getenv(valueName))
	path := strings.TrimSpace(os.Getenv(fileName))
	if value != "" && path != "" {
		return "", errors.New("value and file are both configured")
	}
	if path != "" {
		data, err := readBoundedFile(path, 4096)
		if err != nil {
			return "", err
		}
		value = strings.TrimSpace(string(data))
	}
	if required && value == "" {
		return "", errors.New("required value is empty")
	}
	return value, nil
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(data) == 0 || int64(len(data)) > limit {
		return nil, errors.New("file is empty, too large or unreadable")
	}
	return data, nil
}

func decodePublicKey(value string) (ed25519.PublicKey, error) {
	value = strings.TrimSpace(value)
	if raw, err := hex.DecodeString(value); err == nil && len(raw) == ed25519.PublicKeySize {
		return ed25519.PublicKey(raw), nil
	}
	encodings := []*base64.Encoding{
		base64.RawStdEncoding, base64.RawURLEncoding, base64.StdEncoding, base64.URLEncoding,
	}
	for _, encoding := range encodings {
		if raw, err := encoding.DecodeString(value); err == nil && len(raw) == ed25519.PublicKeySize {
			return ed25519.PublicKey(raw), nil
		}
	}
	return nil, errors.New("public key must encode 32 bytes")
}

func isLoopbackAddress(address string) (bool, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false, err
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false, errors.New("listen host must be a literal IP address")
	}
	return ip.IsLoopback(), nil
}
