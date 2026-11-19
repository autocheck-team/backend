package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestChat(t *testing.T) {
	tests := []struct {
		name, body string
		code       int
		wantError  bool
	}{
		{"success", `{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`, 200, false},
		{"empty", `{"choices":[{"message":{"content":" "}}]}`, 200, true},
		{"invalid_json", `{"choices":[{"message":{"content":"broken"}}]}`, 200, true},
		{"invalid_envelope", `{`, 200, true},
		{"error_envelope", `{"error":{"message":"secret"}}`, 200, true},
		{"truncated", `{"choices":[{"message":{"content":"{}"},"finish_reason":"length"}]}`, 200, true},
		{"array", `{"choices":[{"message":{"content":"[]"}}]}`, 200, true},
		{"no_choices", `{}`, 200, true},
		{"unauthorized", `{"error":"secret"}`, 401, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// arrange
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer test" {
					t.Error("invalid request")
				}
				var request struct {
					Provider struct {
						ZDR bool `json:"zdr"`
					} `json:"provider"`
					MaxTokens      int `json:"max_tokens"`
					ResponseFormat struct {
						Type string `json:"type"`
					} `json:"response_format"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if !request.Provider.ZDR || request.MaxTokens != 8192 || request.ResponseFormat.Type != "json_schema" {
					t.Error("request options missing")
				}
				w.WriteHeader(tc.code)
				if _, err := w.Write([]byte(tc.body)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, APIKey: "test"})
			if err != nil {
				t.Fatal(err)
			}
			// act
			_, err = client.Chat(t.Context(), []Message{{Role: "user", Content: "data"}}, Options{ZDR: true, JSONSchema: json.RawMessage(`{"name":"test","schema":{"type":"object"}}`)})
			// assert
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected error: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("provider message leaked")
			}
		})
	}
}

func TestRetry(t *testing.T) {
	for _, code := range []int{429, 503} {
		t.Run(strings.ToLower(strings.ReplaceAll(http.StatusText(code), " ", "_")), func(t *testing.T) {
			// arrange
			t.Parallel()
			var calls atomic.Int32
			var first time.Time
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					first = time.Now()
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(code)
					if _, err := w.Write([]byte(`{"error":{"retry_after":1.05}}`)); err != nil {
						t.Error(err)
					}
					return
				}
				if time.Since(first) < 1050*time.Millisecond {
					t.Error("retry_after ignored")
				}
				if _, err := w.Write([]byte(`{"choices":[{"message":{"content":"{}"}}]}`)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			client, err := New(Config{BaseURL: server.URL, APIKey: "test", Retries: 1})
			if err != nil {
				t.Fatal(err)
			}
			// act
			_, err = client.Chat(t.Context(), nil, Options{})
			// assert
			if err != nil || calls.Load() != 2 {
				t.Fatalf("calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestRetryCancellation(t *testing.T) {
	// arrange
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("Retry-After", "60"); w.WriteHeader(429) }))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, APIKey: "test", Retries: 2, Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	// act
	_, err = client.Chat(t.Context(), nil, Options{})
	// assert
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEmbeddings(t *testing.T) {
	// arrange
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Error("invalid path")
		}
		if _, err := w.Write([]byte(`{"data":[{"index":1,"embedding":[3,4]},{"index":0,"embedding":[1,2]}]}`)); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, APIKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	// act
	result, err := client.Embed(t.Context(), []string{"a", "b"}, Options{ZDR: true})
	// assert
	if err != nil || len(result) != 2 || result[0][0] != 1 || result[1][0] != 3 {
		t.Fatalf("result=%v err=%v", result, err)
	}
	disabled, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := disabled.Chat(t.Context(), nil, Options{}); !errors.Is(err, ErrDisabled) {
		t.Fatal("disabled chat enabled")
	}
	if _, err := disabled.Embed(t.Context(), []string{"a"}, Options{}); !errors.Is(err, ErrDisabled) {
		t.Fatal("disabled embeddings enabled")
	}
}

// TestOpenRouterLive делает ровно один запрос, без автоматических повторов и вывода ключа.
func TestOpenRouterLive(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		key = os.Getenv("LLM_API_KEY")
	}
	if key == "" {
		t.Skip("OPENROUTER_API_KEY/LLM_API_KEY not set")
	}
	client, err := New(Config{APIKey: key, Model: os.Getenv("LLM_MODEL"), Retries: 0, Timeout: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Chat(t.Context(), []Message{{Role: "system", Content: `Return only JSON: {"ok":true}.`}, {Role: "user", Content: "Check connectivity."}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(result, &response); err != nil || !response.OK {
		t.Fatal("invalid live response")
	}
}
