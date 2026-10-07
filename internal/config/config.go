// Package config читает настройки сервисов из переменных окружения.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config — настройки api, worker и migrate.
type Config struct {
	HTTPAddr        string
	DatabaseURL     string
	ShutdownTimeout time.Duration

	S3Endpoint  string
	S3Bucket    string
	S3AccessKey string
	S3SecretKey string

	WorkerID          string
	WorkerConcurrency int
}

// Load собирает конфиг из окружения. Обязательна только DATABASE_URL,
// остальное имеет значения по умолчанию для локальной разработки.
func Load() (Config, error) {
	c := Config{
		HTTPAddr:    getenv("HTTP_ADDR", ":8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		S3Endpoint:  getenv("S3_ENDPOINT", "localhost:8333"),
		S3Bucket:    getenv("S3_BUCKET", "autocheck"),
		S3AccessKey: os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey: os.Getenv("S3_SECRET_KEY"),
		WorkerID:    getenv("WORKER_ID", hostname()),
	}

	var errs []error
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}

	var err error
	if c.ShutdownTimeout, err = time.ParseDuration(getenv("SHUTDOWN_TIMEOUT", "15s")); err != nil {
		errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT: %w", err))
	}
	if c.WorkerConcurrency, err = strconv.Atoi(getenv("WORKER_CONCURRENCY", "2")); err != nil {
		errs = append(errs, fmt.Errorf("WORKER_CONCURRENCY: %w", err))
	} else if c.WorkerConcurrency < 1 {
		errs = append(errs, errors.New("WORKER_CONCURRENCY must be >= 1"))
	}

	return c, errors.Join(errs...)
}

func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "worker"
	}
	return h
}
