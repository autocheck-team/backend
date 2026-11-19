package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/autocheck")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.YookassaAPIURL != "https://api.yookassa.ru/v3" || c.YookassaTimeout != 10*time.Second || c.BillingSyncInterval != 30*time.Second {
		t.Fatalf("unexpected billing defaults")
	}
	if c.MetricsAddr != ":9100" || c.MetricsPollInterval != 5*time.Second || c.MetricsQueryTimeout != 3*time.Second {
		t.Fatal("unexpected monitoring defaults")
	}
	if c.MaterialQueryTimeout != 5*time.Second || c.MaterialObjectTimeout != 30*time.Second || c.MaterialMaxAttachmentBytes != 10485760 {
		t.Fatal("unexpected material defaults")
	}
	if c.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", c.HTTPAddr)
	}
	if c.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 15s", c.ShutdownTimeout)
	}
	if c.WorkerConcurrency != 2 {
		t.Errorf("WorkerConcurrency = %d, want 2", c.WorkerConcurrency)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"invalid_metrics_interval", map[string]string{"DATABASE_URL": "x", "METRICS_POLL_INTERVAL": "invalid"}},
		{"zero_metrics_interval", map[string]string{"DATABASE_URL": "x", "METRICS_POLL_INTERVAL": "0s"}},
		{"negative_metrics_timeout", map[string]string{"DATABASE_URL": "x", "METRICS_QUERY_TIMEOUT": "-1s"}},
		{"bad_material_query_timeout", map[string]string{"DATABASE_URL": "x", "MATERIAL_QUERY_TIMEOUT": "bad"}},
		{"zero_material_query_timeout", map[string]string{"DATABASE_URL": "x", "MATERIAL_QUERY_TIMEOUT": "0s"}},
		{"bad_material_object_timeout", map[string]string{"DATABASE_URL": "x", "MATERIAL_OBJECT_TIMEOUT": "bad"}},
		{"zero_material_object_timeout", map[string]string{"DATABASE_URL": "x", "MATERIAL_OBJECT_TIMEOUT": "0s"}},
		{"bad_material_limit", map[string]string{"DATABASE_URL": "x", "MATERIAL_MAX_ATTACHMENT_BYTES": "bad"}},
		{"zero_material_limit", map[string]string{"DATABASE_URL": "x", "MATERIAL_MAX_ATTACHMENT_BYTES": "0"}},
		{"excessive_material_limit", map[string]string{"DATABASE_URL": "x", "MATERIAL_MAX_ATTACHMENT_BYTES": "67108865"}},
		{"missing database url", map[string]string{"DATABASE_URL": ""}},
		{"bad shutdown timeout", map[string]string{"DATABASE_URL": "x", "SHUTDOWN_TIMEOUT": "soon"}},
		{"bad yookassa timeout", map[string]string{"DATABASE_URL": "x", "YOOKASSA_TIMEOUT": "no"}},
		{"zero yookassa timeout", map[string]string{"DATABASE_URL": "x", "YOOKASSA_TIMEOUT": "0s"}},
		{"bad billing interval", map[string]string{"DATABASE_URL": "x", "BILLING_SYNC_INTERVAL": "0s"}},
		{"bad concurrency", map[string]string{"DATABASE_URL": "x", "WORKER_CONCURRENCY": "0"}},
		{"bad_playground_timeout", map[string]string{"DATABASE_URL": "x", "PLAYGROUND_QUERY_TIMEOUT": "soon"}},
		{"zero_playground_timeout", map[string]string{"DATABASE_URL": "x", "PLAYGROUND_QUERY_TIMEOUT": "0s"}},
		{"negative_playground_timeout", map[string]string{"DATABASE_URL": "x", "PLAYGROUND_QUERY_TIMEOUT": "-1s"}},
		{"bad_feedback_timeout", map[string]string{"DATABASE_URL": "x", "FEEDBACK_QUERY_TIMEOUT": "soon"}},
		{"zero_feedback_timeout", map[string]string{"DATABASE_URL": "x", "FEEDBACK_QUERY_TIMEOUT": "0s"}},
		{"negative_feedback_timeout", map[string]string{"DATABASE_URL": "x", "FEEDBACK_QUERY_TIMEOUT": "-1s"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			if _, err := Load(); err == nil {
				t.Fatal("Load: expected error")
			}
		})
	}
}
