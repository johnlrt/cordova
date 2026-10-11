package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cordova/server/internal/ranker"
	"github.com/redis/go-redis/v9"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	redisURL := strings.TrimSpace(os.Getenv("CORDOVA_REDIS_URL"))
	if redisURL == "" {
		return errors.New("CORDOVA_REDIS_URL is required")
	}
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		return errors.New("CORDOVA_REDIS_URL is invalid")
	}
	if passwordFile := strings.TrimSpace(os.Getenv("CORDOVA_REDIS_PASSWORD_FILE")); passwordFile != "" {
		password, err := os.ReadFile(passwordFile)
		if err != nil || strings.TrimSpace(string(password)) == "" {
			return errors.New("Redis password file is unavailable")
		}
		options.Password = strings.TrimSpace(string(password))
	}
	if os.Getenv("CORDOVA_ENV") != "development" && options.TLSConfig == nil {
		return errors.New("Redis TLS is required outside development mode")
	}
	rdb := redis.NewClient(options)
	defer rdb.Close()
	worker, err := ranker.NewWorker(rdb, strings.TrimSpace(os.Getenv("CORDOVA_KAFKA_BROKERS")))
	if err != nil {
		return err
	}
	defer worker.Close()
	startupCtx, startupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := worker.Ping(startupCtx); err != nil {
		startupCancel()
		return err
	}
	startupCancel()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Print("Cordova ranker consuming validated node metadata; no request content is stored")
	return worker.Run(ctx)
}
