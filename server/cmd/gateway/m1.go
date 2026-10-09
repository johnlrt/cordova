package main

import (
	"context"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cordova/server/internal/m1"
	"cordova/server/internal/mocknode"

	"github.com/redis/go-redis/v9"
)

func runM1(address string) error {
	origin := strings.TrimSpace(os.Getenv("CORDOVA_PUBLIC_ORIGIN"))
	if !validPublicOrigin(origin) {
		return errors.New("CORDOVA_PUBLIC_ORIGIN must be an absolute HTTP(S) origin without a path")
	}
	notice := strings.TrimSpace(os.Getenv("CORDOVA_PRIVACY_NOTICE_VERSION"))
	if notice == "" {
		notice = "v1"
	}
	pepper, err := secretBytes("CORDOVA_APP_PEPPER", "CORDOVA_APP_PEPPER_FILE")
	if err != nil {
		return err
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
	config := m1.Config{Origin: origin, PrivacyNoticeVersion: notice, Pepper: pepper, SecureCookies: true, TrustProxy: strings.EqualFold(os.Getenv("CORDOVA_TRUST_PROXY"), "true")}
	store, err := m1.NewStore(rdb, config)
	if err != nil {
		return err
	}
	brokers := strings.TrimSpace(os.Getenv("CORDOVA_KAFKA_BROKERS"))
	events, err := m1.NewEvents(rdb, brokers)
	if err != nil {
		return err
	}
	defer events.Close()
	endpoint := strings.TrimSpace(os.Getenv("CORDOVA_MOCK_NODE_URL"))
	if endpoint == "" {
		return errors.New("CORDOVA_MOCK_NODE_URL is required in M1 mode")
	}
	token, err := secretBytes("CORDOVA_MOCK_NODE_TOKEN", "CORDOVA_MOCK_NODE_TOKEN_FILE")
	if err != nil {
		return err
	}
	caPath := strings.TrimSpace(os.Getenv("CORDOVA_MOCK_NODE_CA_FILE"))
	if caPath == "" {
		return errors.New("CORDOVA_MOCK_NODE_CA_FILE is required in M1 mode")
	}
	caData, err := os.ReadFile(caPath)
	if err != nil {
		return errors.New("mock node CA could not be read")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caData) {
		return errors.New("mock node CA is invalid")
	}
	provider, err := mocknode.NewProvider(endpoint, string(token), roots)
	if err != nil {
		return err
	}
	startupCtx, startupCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer startupCancel()
	for {
		redisErr := rdb.Ping(startupCtx).Err()
		kafkaErr := events.Ping(startupCtx)
		nodeErr := provider.Ping(startupCtx)
		if redisErr == nil && kafkaErr == nil && nodeErr == nil {
			break
		}
		if startupCtx.Err() != nil {
			return errors.New("M1 dependencies are not ready")
		}
		time.Sleep(500 * time.Millisecond)
	}
	handler, err := m1.NewHandler(config, store, provider, func(ctx context.Context) error {
		if err := rdb.Ping(ctx).Err(); err != nil {
			return errors.New("Redis is unavailable")
		}
		if err := events.Ping(ctx); err != nil {
			return err
		}
		return provider.Ping(ctx)
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
	fmt.Printf("Cordova M1 mock slice listening on %s; synthetic usage only; test credits only\n", address)
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
