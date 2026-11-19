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
	MetricsAddr          string
	MetricsPollInterval  time.Duration
	MetricsQueryTimeout  time.Duration
	HTTPAddr             string
	GRPCAddr             string
	DatabaseURL          string
	ShutdownTimeout      time.Duration
	FeedbackQueryTimeout time.Duration

	S3Endpoint string
	// S3PublicEndpoint — адрес хранилища для браузера (presigned-ссылки).
	S3PublicEndpoint string
	S3Bucket         string
	S3AccessKey      string
	S3SecretKey      string

	WorkerID           string
	WorkerConcurrency  int
	WorkerPollInterval time.Duration
	SandboxRuntime     string

	// OIDCIssuer — ожидаемый claim iss в токенах Keycloak (адрес realm, как его видит браузер).
	OIDCIssuer string
	// OIDCJWKSURL — откуда брать ключи подписи; внутри compose хост отличается от issuer.
	OIDCJWKSURL  string
	OIDCAudience string

	YookassaShopID      string
	YookassaSecretKey   string
	YookassaAPIURL      string
	YookassaTimeout     time.Duration
	BillingSyncInterval time.Duration

	CORSAllowedOrigins []string
}

// Load собирает конфиг из окружения. Обязательна только DATABASE_URL,
// остальное имеет значения по умолчанию для локальной разработки.
func Load() (Config, error) {
	c := Config{
		YookassaShopID:    os.Getenv("YOOKASSA_SHOP_ID"),
		YookassaSecretKey: os.Getenv("YOOKASSA_SECRET_KEY"),
		YookassaAPIURL:    getenv("YOOKASSA_API_URL", "https://api.yookassa.ru/v3"),
		MetricsAddr:       getenv("METRICS_ADDR", ":9100"),
		HTTPAddr:          getenv("HTTP_ADDR", ":8080"),
		GRPCAddr:          getenv("GRPC_ADDR", ":9090"),
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		S3Endpoint:        getenv("S3_ENDPOINT", "localhost:8333"),
		S3Bucket:          getenv("S3_BUCKET", "autocheck"),
		S3PublicEndpoint:  os.Getenv("S3_PUBLIC_ENDPOINT"),
		S3AccessKey:       os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:       os.Getenv("S3_SECRET_KEY"),
		WorkerID:          getenv("WORKER_ID", hostname()),
		SandboxRuntime:    os.Getenv("SANDBOX_RUNTIME"),

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
	if c.FeedbackQueryTimeout, err = time.ParseDuration(getenv("FEEDBACK_QUERY_TIMEOUT", "5s")); err != nil {
		errs = append(errs, fmt.Errorf("invalid feedback query timeout: %w", err))
	} else if c.FeedbackQueryTimeout <= 0 {
		errs = append(errs, errors.New("feedback query timeout must be positive"))
	}
	if c.WorkerConcurrency, err = strconv.Atoi(getenv("WORKER_CONCURRENCY", "2")); err != nil {
		errs = append(errs, fmt.Errorf("WORKER_CONCURRENCY: %w", err))
	} else if c.WorkerConcurrency < 1 {
		errs = append(errs, errors.New("WORKER_CONCURRENCY must be >= 1"))
	}

	if c.WorkerPollInterval, err = time.ParseDuration(getenv("WORKER_POLL_INTERVAL", "1s")); err != nil {
		errs = append(errs, fmt.Errorf("WORKER_POLL_INTERVAL: %w", err))
	} else if c.WorkerPollInterval <= 0 {
		errs = append(errs, errors.New("WORKER_POLL_INTERVAL must be positive"))
	}
	if c.YookassaTimeout, err = time.ParseDuration(getenv("YOOKASSA_TIMEOUT", "10s")); err != nil {
		errs = append(errs, fmt.Errorf("YOOKASSA_TIMEOUT: %w", err))
	} else if c.YookassaTimeout <= 0 {
		errs = append(errs, errors.New("YOOKASSA_TIMEOUT must be positive"))
	}
	if c.BillingSyncInterval, err = time.ParseDuration(getenv("BILLING_SYNC_INTERVAL", "30s")); err != nil {
		errs = append(errs, fmt.Errorf("BILLING_SYNC_INTERVAL: %w", err))
	} else if c.BillingSyncInterval <= 0 {
		errs = append(errs, errors.New("BILLING_SYNC_INTERVAL must be positive"))
	}
	for _, setting := range []struct {
		name     string
		fallback string
		target   *time.Duration
	}{
		{"METRICS_POLL_INTERVAL", "5s", &c.MetricsPollInterval},
		{"METRICS_QUERY_TIMEOUT", "3s", &c.MetricsQueryTimeout},
	} {
		duration, parseErr := time.ParseDuration(getenv(setting.name, setting.fallback))
		if parseErr != nil {
			errs = append(errs, fmt.Errorf("parse %s: %w", setting.name, parseErr))
			continue
		}
		if duration <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive", setting.name))
			continue
		}
		*setting.target = duration
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
