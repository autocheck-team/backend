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
		{"bad yookassa timeout", map[string]string{"DATABASE_URL": "x", "YOOKASSA_TIMEOUT": "no"}},
		{"zero yookassa timeout", map[string]string{"DATABASE_URL": "x", "YOOKASSA_TIMEOUT": "0s"}},
		{"bad billing interval", map[string]string{"DATABASE_URL": "x", "BILLING_SYNC_INTERVAL": "0s"}},
		{"bad concurrency", map[string]string{"DATABASE_URL": "x", "WORKER_CONCURRENCY": "0"}},
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

func TestLLMConfig(t *testing.T) {
	// arrange
	t.Setenv("DATABASE_URL", "postgres://localhost/autocheck")
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("LLM_BASE_URL", "")
	t.Setenv("LLM_MODEL", "")
	t.Setenv("LLM_EMBEDDING_MODEL", "")
	// act
	cfg, err := Load()
	// assert
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.Model != "nvidia/nemotron-3.5-super-vl:free" || cfg.LLM.EmbeddingModel != "nvidia/nemotron-3.5-embed-1b:free" || cfg.LLM.MaxTokens != 8192 || cfg.LLM.Retries != 2 || cfg.LLM.Timeout != 2*time.Minute {
		t.Fatal("unexpected LLM defaults")
	}
	for _, tc := range []struct{ name, key, value string }{
		{"bad_timeout", "LLM_TIMEOUT", "bad"},
		{"zero_timeout", "LLM_TIMEOUT", "0s"},
		{"negative_timeout", "LLM_TIMEOUT", "-1s"},
		{"bad_retries", "LLM_RETRIES", "bad"},
		{"negative_retries", "LLM_RETRIES", "-1"},
		{"excess_retries", "LLM_RETRIES", "6"},
		{"bad_tokens", "LLM_MAX_TOKENS", "bad"},
		{"zero_tokens", "LLM_MAX_TOKENS", "0"},
		{"negative_tokens", "LLM_MAX_TOKENS", "-1"},
		{"invalid_url", "LLM_BASE_URL", "file:///tmp/llm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatal("invalid LLM configuration accepted")
			}
		})
	}
}
