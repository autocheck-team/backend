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
		{"missing database url", map[string]string{"DATABASE_URL": ""}},
		{"bad shutdown timeout", map[string]string{"DATABASE_URL": "x", "SHUTDOWN_TIMEOUT": "soon"}},
		{"bad concurrency", map[string]string{"DATABASE_URL": "x", "WORKER_CONCURRENCY": "0"}},
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
