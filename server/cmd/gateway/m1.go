package main

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cordova/server/internal/billing"
	"cordova/server/internal/domain"
	"cordova/server/internal/m1"
	"cordova/server/internal/mocknode"
	"cordova/server/internal/request"
	trustedtokenizer "cordova/server/internal/tokenizer"

	"github.com/redis/go-redis/v9"
)

func runM1(address string) error {
	inferenceMode := strings.ToLower(strings.TrimSpace(os.Getenv("CORDOVA_INFERENCE_MODE")))
	if inferenceMode == "" {
		inferenceMode = "mock"
	}
	if inferenceMode != "mock" && inferenceMode != "c3" {
		return errors.New("CORDOVA_INFERENCE_MODE must be mock or c3")
	}
	origin := strings.TrimSpace(os.Getenv("CORDOVA_PUBLIC_ORIGIN"))
	if !validPublicOrigin(origin) {
		return errors.New("CORDOVA_PUBLIC_ORIGIN must be an absolute HTTP(S) origin without a path")
	}
	notice := strings.TrimSpace(os.Getenv("CORDOVA_PRIVACY_NOTICE_VERSION"))
	if notice == "" {
		notice = m1.DefaultPrivacyNoticeVersion
	}
	pepper, err := secretBytes("CORDOVA_APP_PEPPER", "CORDOVA_APP_PEPPER_FILE")
	if err != nil {
		return err
	}
	nodeEncryptionKey, nodeSigningKey, err := nodeAPIKeys()
	if err != nil {
		return err
	}
	catalogSnapshot, catalogPublicKey, err := nodeCatalogConfig()
	if err != nil {
		return err
	}
	tokenizerEndpoint := strings.TrimSpace(os.Getenv("CORDOVA_TOKENIZER_URL"))
	tokenizerToken, tokenizerTokenSet, err := optionalSecretText("CORDOVA_TOKENIZER_TOKEN", "CORDOVA_TOKENIZER_TOKEN_FILE")
	if err != nil {
		return err
	}
	tokenizerRoots, err := tokenizerTLSRoots()
	if err != nil {
		return err
	}
	inputTokenizer, trustedTokenizerEnabled, err := configuredTokenizer(tokenizerEndpoint, tokenizerToken, tokenizerTokenSet, catalogSnapshot, catalogPublicKey, tokenizerRoots)
	if err != nil {
		return err
	}
	if inferenceMode == "c3" && !trustedTokenizerEnabled {
		return errors.New("C3 inference requires the trusted tokenizer and a signed model catalog")
	}
	redisURL := strings.TrimSpace(os.Getenv("CORDOVA_REDIS_URL"))
	if redisURL == "" {
		return errors.New("CORDOVA_REDIS_URL is required in M1 mode")
	}
	parsedRedis, err := redis.ParseURL(redisURL)
	if err != nil {
		return errors.New("CORDOVA_REDIS_URL is invalid")
	}
	if passwordFile := strings.TrimSpace(os.Getenv("CORDOVA_REDIS_PASSWORD_FILE")); passwordFile != "" {
		password, err := os.ReadFile(passwordFile)
		if err != nil || strings.TrimSpace(string(password)) == "" {
			return errors.New("Redis password file is unavailable")
		}
		parsedRedis.Password = strings.TrimSpace(string(password))
	}
	if os.Getenv("CORDOVA_ENV") != "development" && parsedRedis.TLSConfig == nil {
		return errors.New("Redis TLS is required outside development mode")
	}
	rdb := redis.NewClient(parsedRedis)
	defer rdb.Close()
	config := m1.Config{Origin: origin, PrivacyNoticeVersion: notice, Pepper: pepper,
		NodeSecretEncryptionKey: nodeEncryptionKey, NodeJWTSigningKey: nodeSigningKey,
		NodeCatalogSnapshot: catalogSnapshot, NodeCatalogPublicKey: catalogPublicKey,
		InferenceMode: inferenceMode,
		SecureCookies: true, TrustProxy: strings.EqualFold(os.Getenv("CORDOVA_TRUST_PROXY"), "true")}
	store, err := m1.NewStore(rdb, config)
	if err != nil {
		return err
	}
	journal, err := billing.NewRedisJournal(rdb, os.Getenv("CORDOVA_ENV") != "development")
	if err != nil {
		return err
	}
	ledger, err := billing.NewRedisLedger(journal)
	if err != nil {
		return err
	}
	brokers := strings.TrimSpace(os.Getenv("CORDOVA_KAFKA_BROKERS"))
	events, err := m1.NewEvents(rdb, brokers)
	if err != nil {
		return err
	}
	defer events.Close()
	var provider *mocknode.Provider
	var router request.Router
	var routerErr error
	switch inferenceMode {
	case "mock":
		endpoint := strings.TrimSpace(os.Getenv("CORDOVA_MOCK_NODE_URL"))
		if endpoint == "" {
			return errors.New("CORDOVA_MOCK_NODE_URL is required in mock inference mode")
		}
		token, err := secretBytes("CORDOVA_MOCK_NODE_TOKEN", "CORDOVA_MOCK_NODE_TOKEN_FILE")
		if err != nil {
			return err
		}
		caPath := strings.TrimSpace(os.Getenv("CORDOVA_MOCK_NODE_CA_FILE"))
		if caPath == "" {
			return errors.New("CORDOVA_MOCK_NODE_CA_FILE is required in mock inference mode")
		}
		caData, err := os.ReadFile(caPath)
		if err != nil {
			return errors.New("mock node CA could not be read")
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caData) {
			return errors.New("mock node CA is invalid")
		}
		provider, routerErr = mocknode.NewProvider(endpoint, string(token), roots)
		if routerErr != nil {
			return routerErr
		}
		router, routerErr = mocknode.NewRequestRouter(provider, "00000000-0000-4000-8000-000000000002", 4)
	case "c3":
		if len(nodeEncryptionKey) != 32 || len(nodeSigningKey) != ed25519.PrivateKeySize {
			return errors.New("C3 inference requires the node API encryption and signing keys")
		}
		router, routerErr = m1.NewC3RequestRouter(store)
	}
	if routerErr != nil {
		return routerErr
	}
	pingInference := func(ctx context.Context) error {
		if provider == nil {
			return nil
		}
		return provider.Ping(ctx)
	}
	priceVersion, err := domain.ParseID("00000000-0000-4000-8000-000000000001")
	if err != nil {
		return err
	}
	zeroRate, err := domain.ParsePiconero("0")
	if err != nil {
		return err
	}
	coordinator, err := request.NewCoordinator(ledger, inputTokenizer, router)
	if err != nil {
		return err
	}
	executor, err := m1.NewAccountedExecutor(coordinator, billing.Price{Version: priceVersion,
		InputRate: zeroRate, OutputRate: zeroRate, OwnerShareBPS: 0})
	if err != nil {
		return err
	}
	startupCtx, startupCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer startupCancel()
	for {
		redisErr := rdb.Ping(startupCtx).Err()
		kafkaErr := events.Ping(startupCtx)
		nodeErr := pingInference(startupCtx)
		tokenizerErr := checkTokenizerReady(startupCtx, inputTokenizer)
		if redisErr == nil && kafkaErr == nil && nodeErr == nil && tokenizerErr == nil {
			break
		}
		if startupCtx.Err() != nil {
			return errors.New("M1 dependencies are not ready")
		}
		time.Sleep(500 * time.Millisecond)
	}
	if _, err := ledger.Recover(startupCtx, time.Now().UTC()); err != nil {
		return errors.New("M3 ledger recovery did not complete; gateway remains unavailable")
	}
	if err := store.ReconcileIdempotency(startupCtx, ledger); err != nil {
		return errors.New("M3 idempotency recovery did not complete; gateway remains unavailable")
	}
	if err := store.ReconcileAccountedUsage(startupCtx, ledger); err != nil {
		return errors.New("M3 usage projection recovery did not complete; gateway remains unavailable")
	}
	handler, err := m1.NewHandlerWithLedgerAndExecutor(config, store, executor, ledger, func(ctx context.Context) error {
		if err := rdb.Ping(ctx).Err(); err != nil {
			return errors.New("Redis is unavailable")
		}
		if err := events.Ping(ctx); err != nil {
			return err
		}
		if err := checkTokenizerReady(ctx, inputTokenizer); err != nil {
			return errors.New("trusted tokenizer is unavailable")
		}
		if !ledger.Ready() {
			return errors.New("financial ledger recovery is incomplete")
		}
		return pingInference(ctx)
	})
	if err != nil {
		return err
	}
	workerCtx, stopWorkers := context.WithCancel(context.Background())
	defer stopWorkers()
	workerResult := make(chan error, 1)
	go func() { workerResult <- events.Run(workerCtx) }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := m1HTTPServer(address, handler)
	serverResult := make(chan error, 1)
	go func() { serverResult <- server.ListenAndServe() }()
	if trustedTokenizerEnabled {
		fmt.Printf("Cordova M1 %s inference listening on %s; trusted tokenizer enabled; test credits only\n", inferenceMode, address)
	} else {
		fmt.Printf("Cordova M1 %s inference listening on %s; synthetic usage only; test credits only\n", inferenceMode, address)
	}
	select {
	case err := <-serverResult:
		stopWorkers()
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("gateway listener failed")
		}
	case err := <-workerResult:
		stopWorkers()
		_ = server.Close()
		if err != nil {
			return fmt.Errorf("M1 event worker stopped: %w", err)
		}
	case <-ctx.Done():
		stopWorkers()
		shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return errors.New("gateway shutdown failed")
		}
	}
	return nil
}

func m1HTTPServer(address string, handler http.Handler) *http.Server {
	return &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
}

func validPublicOrigin(value string) bool {
	u, err := url.Parse(value)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && u.User == nil
}

func secretBytes(envName, fileName string) ([]byte, error) {
	if file := strings.TrimSpace(os.Getenv(fileName)); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", fileName, err)
		}
		value := strings.TrimSpace(string(data))
		if value == "" {
			return nil, errors.New(fileName + " is empty")
		}
		if envName == "CORDOVA_APP_PEPPER" && len(value) == 64 {
			decoded, err := hex.DecodeString(value)
			if err == nil {
				return decoded, nil
			}
		}
		return []byte(value), nil
	}
	value := strings.TrimSpace(os.Getenv(envName))
	if value == "" {
		return nil, errors.New(envName + " or " + fileName + " is required")
	}
	return []byte(value), nil
}

func nodeAPIKeys() ([]byte, ed25519.PrivateKey, error) {
	encryptionText, encryptionSet, err := optionalSecretText("CORDOVA_NODE_SECRET_ENCRYPTION_KEY", "CORDOVA_NODE_SECRET_ENCRYPTION_KEY_FILE")
	if err != nil {
		return nil, nil, err
	}
	signingText, signingSet, err := optionalSecretText("CORDOVA_NODE_JWT_SIGNING_KEY", "CORDOVA_NODE_JWT_SIGNING_KEY_FILE")
	if err != nil {
		return nil, nil, err
	}
	if !encryptionSet && !signingSet {
		return nil, nil, nil
	}
	if !encryptionSet || !signingSet {
		return nil, nil, errors.New("both node encryption and JWT signing keys are required to enable the node API")
	}
	encryptionKey, err := decodeFixedKey(encryptionText, 32)
	if err != nil {
		return nil, nil, errors.New("node encryption key must encode exactly 32 bytes")
	}
	signingBytes, err := decodeFixedKey(signingText, ed25519.PrivateKeySize)
	if err == nil {
		return encryptionKey, ed25519.PrivateKey(signingBytes), nil
	}
	seed, err := decodeFixedKey(signingText, ed25519.SeedSize)
	if err != nil {
		return nil, nil, errors.New("node JWT signing key must encode an Ed25519 seed or private key")
	}
	return encryptionKey, ed25519.NewKeyFromSeed(seed), nil
}

func nodeCatalogConfig() ([]byte, ed25519.PublicKey, error) {
	path := strings.TrimSpace(os.Getenv("CORDOVA_MODEL_CATALOG_FILE"))
	keyText, keySet, err := optionalSecretText("CORDOVA_MODEL_CATALOG_PUBLIC_KEY", "CORDOVA_MODEL_CATALOG_PUBLIC_KEY_FILE")
	if err != nil {
		return nil, nil, err
	}
	if path == "" && !keySet {
		return nil, nil, nil
	}
	if path == "" || !keySet {
		return nil, nil, errors.New("CORDOVA_MODEL_CATALOG_FILE and CORDOVA_MODEL_CATALOG_PUBLIC_KEY (or _FILE) must be configured together")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, errors.New("model catalog file could not be read")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, m1.MaxNodeCatalogBytes+1))
	if err != nil || len(data) > m1.MaxNodeCatalogBytes {
		return nil, nil, errors.New("model catalog file exceeds the safe WebSocket frame limit or could not be read")
	}
	if len(data) == 0 {
		return nil, nil, errors.New("model catalog file is empty")
	}
	key, err := decodeFixedKey(keyText, ed25519.PublicKeySize)
	if err != nil {
		return nil, nil, errors.New("model catalog public key must encode exactly 32 bytes")
	}
	return data, ed25519.PublicKey(key), nil
}

func configuredTokenizer(endpoint, token string, tokenConfigured bool, catalogData []byte, catalogKey ed25519.PublicKey, roots *x509.CertPool) (request.Tokenizer, bool, error) {
	if endpoint == "" && !tokenConfigured {
		return mocknode.SyntheticTokenizer{}, false, nil
	}
	if endpoint == "" || !tokenConfigured {
		return nil, false, errors.New("CORDOVA_TOKENIZER_URL and CORDOVA_TOKENIZER_TOKEN (or _FILE) must be configured together")
	}
	catalog, err := m1.ParseNodeCatalog(catalogData, catalogKey)
	if err != nil {
		return nil, false, fmt.Errorf("trusted tokenizer requires a valid signed model catalog: %w", err)
	}
	if len(catalog.Models) == 0 {
		return nil, false, errors.New("trusted tokenizer requires at least one model in the signed model catalog")
	}
	models := make([]trustedtokenizer.Model, 0, len(catalog.Models))
	for _, manifest := range catalog.Models {
		models = append(models, trustedtokenizer.Model{
			ID: manifest.Model.ID, CatalogVersion: catalog.Version,
			ContextTokens: manifest.Model.ContextTokens,
			AddBOS:        manifest.AddBOS,
			ArtifactURL:   manifest.ArtifactURL, ArtifactSHA256: manifest.ArtifactSHA256,
			ArtifactSizeBytes: manifest.ArtifactSizeBytes, LlamaCommit: manifest.LlamaCommit,
			TokenizerSHA256: manifest.TokenizerSHA256, TemplateSHA256: manifest.TemplateSHA256,
		})
	}
	client, err := trustedtokenizer.NewClient(endpoint, token, models, roots)
	if err != nil {
		return nil, false, fmt.Errorf("configure trusted tokenizer: %w", err)
	}
	return client, true, nil
}

func tokenizerTLSRoots() (*x509.CertPool, error) {
	path := strings.TrimSpace(os.Getenv("CORDOVA_TOKENIZER_CA_FILE"))
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("tokenizer CA file could not be read")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(data) == 0 || len(data) > 1<<20 {
		return nil, errors.New("tokenizer CA file is empty, too large or could not be read")
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(data) {
		return nil, errors.New("tokenizer CA file contains no valid certificates")
	}
	return roots, nil
}

func checkTokenizerReady(ctx context.Context, tokenizer request.Tokenizer) error {
	checker, ok := tokenizer.(interface{ CheckReady(context.Context) error })
	if !ok {
		return nil
	}
	return checker.CheckReady(ctx)
}

func optionalSecretText(envName, fileName string) (string, bool, error) {
	if file := strings.TrimSpace(os.Getenv(fileName)); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", false, fmt.Errorf("read %s: %w", fileName, err)
		}
		value := strings.TrimSpace(string(data))
		if value == "" {
			return "", false, errors.New(fileName + " is empty")
		}
		return value, true, nil
	}
	value := strings.TrimSpace(os.Getenv(envName))
	return value, value != "", nil
}

func decodeFixedKey(value string, size int) ([]byte, error) {
	if decoded, err := hex.DecodeString(value); err == nil && len(decoded) == size {
		return decoded, nil
	}
	for _, encoding := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding, base64.StdEncoding, base64.URLEncoding} {
		if decoded, err := encoding.DecodeString(value); err == nil && len(decoded) == size {
			return decoded, nil
		}
	}
	return nil, errors.New("encoded key has the wrong size")
}
