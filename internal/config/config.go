// Package config читает настройки сервисов из переменных окружения.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config — настройки api, worker и migrate.
type Config struct {
	HTTPAddr        string
	GRPCAddr        string
	DatabaseURL     string
	ShutdownTimeout time.Duration

	S3Endpoint  string
	S3Bucket    string
	S3AccessKey string
	S3SecretKey string

	WorkerID          string
	WorkerConcurrency int

	// OIDCIssuer — ожидаемый claim iss в токенах Keycloak (адрес realm, как его видит браузер).
	OIDCIssuer string
	// OIDCJWKSURL — откуда брать ключи подписи; внутри compose хост отличается от issuer.
	OIDCJWKSURL  string
	OIDCAudience string

	CORSAllowedOrigins []string
}

// Load собирает конфиг из окружения. Обязательна только DATABASE_URL,
// остальное имеет значения по умолчанию для локальной разработки.
func Load() (Config, error) {
	c := Config{
		HTTPAddr:    getenv("HTTP_ADDR", ":8080"),
		GRPCAddr:    getenv("GRPC_ADDR", ":9090"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		S3Endpoint:  getenv("S3_ENDPOINT", "localhost:8333"),
		S3Bucket:    getenv("S3_BUCKET", "autocheck"),
		S3AccessKey: os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey: os.Getenv("S3_SECRET_KEY"),
		WorkerID:    getenv("WORKER_ID", hostname()),

		OIDCIssuer:   os.Getenv("OIDC_ISSUER"),
		OIDCAudience: getenv("OIDC_AUDIENCE", "autocheck-api"),
	}
	c.OIDCJWKSURL = getenv("OIDC_JWKS_URL", strings.TrimSuffix(c.OIDCIssuer, "/")+"/protocol/openid-connect/certs")
	for o := range strings.SplitSeq(os.Getenv("CORS_ALLOWED_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			c.CORSAllowedOrigins = append(c.CORSAllowedOrigins, o)
		}
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

// RequireAPI проверяет настройки, обязательные только для сервиса api.
func (c Config) RequireAPI() error {
	if c.OIDCIssuer == "" {
		return errors.New("OIDC_ISSUER is required")
	}
	return nil
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
