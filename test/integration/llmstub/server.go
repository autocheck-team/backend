// Package main запускает локальный детерминированный OpenAI-совместимый сервер.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer integration-only" {
		http.Error(w, "invalid test request", http.StatusBadRequest)
		return
	}
	var request struct {
		Input    []string `json:"input"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&request); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	var response any
	switch r.URL.Path {
	case "/v1/embeddings":
		data := make([]any, 0, len(request.Input))
		for i := range request.Input {
			data = append(data, map[string]any{"index": i, "embedding": []float64{1, 0, 0}})
		}
		response = map[string]any{"data": data}
	case "/v1/chat/completions":
		if len(request.Messages) < 2 || request.Messages[0].Role != "system" {
			http.Error(w, "missing instructions", http.StatusBadRequest)
			return
		}
		system := request.Messages[0].Content
		var content string
		switch {
		case strings.Contains(system, "test INPUTS"):
			content = `{"tests":[{"input":"2 3\n","generator":"","explanation":"Небольшие положительные числа"}]}`
		case strings.Contains(system, `"hint"`):
			content = `{"hint":"Какие значения вы прочитали и что выводите для небольшого примера?"}`
		case strings.Contains(system, `"explanation"`):
			content = `{"explanation":"Эти задания помогают потренироваться на небольших примерах."}`
		default:
			http.Error(w, "unknown instructions", http.StatusBadRequest)
			return
		}
		response = map[string]any{"choices": []any{map[string]any{
			"message": map[string]string{"role": "assistant", "content": content}, "finish_reason": "stop",
		}}}
	default:
		http.NotFound(w, r)
		return
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		// Ответ уже начат; разрыв соединения не должен останавливать остальные тесты.
		return
	}
}

func run(ctx context.Context) error {
	server := &http.Server{
		Addr: ":8099", Handler: http.HandlerFunc(handler), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			return err
		}
		err := <-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil {
		os.Exit(1)
	}
}
