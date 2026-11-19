// Package llm предоставляет общий OpenAI-совместимый клиент без логирования содержимого запросов.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrDisabled означает, что оператор не настроил ключ провайдера.
var ErrDisabled = errors.New("llm is disabled")

// Config задаёт общий deadline запроса вместе с повторами; Retries — число повторов после первой попытки.
type Config struct {
	BaseURL, APIKey, Model, EmbeddingModel string
	Timeout                                time.Duration
	Retries, MaxTokens                     int
}

// Message передаёт инструкции и недоверенные данные отдельными сообщениями.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Options выбирает приватность и строгую схему ответа конкретного вызова.
// ZDR передаётся провайдеру явно, включая embeddings; отсутствие подходящего endpoint не вызывает fallback.
// JSONSchema содержит объект {name, strict, schema}; nil выбирает json_object.
type Options struct {
	ZDR        bool
	JSONSchema json.RawMessage
}

// Client безопасен для совместного использования горутинами.
type Client struct {
	cfg  Config
	http *http.Client
}

// New проверяет конфигурацию; пустой ключ создаёт выключенный клиент.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://openrouter.ai/api/v1"
	}
	if cfg.Model == "" {
		cfg.Model = "nvidia/nemotron-3.5-super-vl:free"
	}
	if cfg.EmbeddingModel == "" {
		cfg.EmbeddingModel = "nvidia/nemotron-3.5-embed-1b:free"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Minute
	}
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = 8192
	}
	parsed, err := url.Parse(cfg.BaseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid llm base URL")
	}
	if cfg.Timeout <= 0 || cfg.Retries < 0 || cfg.Retries > 5 || cfg.MaxTokens < 1 || cfg.MaxTokens > 65536 {
		return nil, errors.New("invalid llm limits")
	}
	return &Client{cfg: cfg, http: &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Enabled сообщает доступность настройки, не обращаясь к провайдеру.
func (c *Client) Enabled() bool { return c != nil && c.cfg.APIKey != "" }

// Chat возвращает проверенный непустой JSON-объект; max_tokens включает запас для reasoning.
func (c *Client) Chat(ctx context.Context, messages []Message, options Options) (json.RawMessage, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	format := map[string]any{"type": "json_object"}
	if len(options.JSONSchema) > 0 {
		if !json.Valid(options.JSONSchema) {
			return nil, errors.New("invalid response schema")
		}
		format = map[string]any{"type": "json_schema", "json_schema": options.JSONSchema}
	}
	var response struct {
		Choices []struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
	}
	err := c.request(ctx, "/chat/completions", map[string]any{
		"model": c.cfg.Model, "messages": messages, "max_tokens": c.cfg.MaxTokens,
		"response_format": format, "provider": map[string]bool{"zdr": options.ZDR},
	}, &response)
	if err != nil {
		return nil, err
	}
	if len(response.Choices) == 0 || strings.TrimSpace(response.Choices[0].Message.Content) == "" {
		return nil, errors.New("empty llm response")
	}
	if response.Choices[0].FinishReason == "length" {
		return nil, errors.New("truncated llm response")
	}
	content := json.RawMessage(response.Choices[0].Message.Content)
	var object map[string]json.RawMessage
	if err := json.Unmarshal(content, &object); err != nil || object == nil {
		return nil, errors.New("invalid llm JSON object")
	}
	return content, nil
}

// Embed возвращает векторы в порядке входов; ZDR действует на весь batch.
func (c *Client) Embed(ctx context.Context, inputs []string, options Options) ([][]float64, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	if len(inputs) == 0 {
		return nil, errors.New("empty embedding inputs")
	}
	var response struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := c.request(ctx, "/embeddings", map[string]any{
		"model": c.cfg.EmbeddingModel, "input": inputs, "provider": map[string]bool{"zdr": options.ZDR},
	}, &response); err != nil {
		return nil, err
	}
	if len(response.Data) != len(inputs) {
		return nil, errors.New("incomplete embedding response")
	}
	result := make([][]float64, len(inputs))
	dimensions := 0
	for _, item := range response.Data {
		if item.Index < 0 || item.Index >= len(result) || len(item.Embedding) == 0 || result[item.Index] != nil {
			return nil, errors.New("invalid embedding response")
		}
		if dimensions > 0 && dimensions != len(item.Embedding) {
			return nil, errors.New("inconsistent embedding dimensions")
		}
		dimensions = len(item.Embedding)
		result[item.Index] = item.Embedding
	}
	return result, nil
}

func (c *Client) request(parent context.Context, path string, payload, target any) error {
	ctx, cancel := context.WithTimeout(parent, c.cfg.Timeout)
	defer cancel()
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode llm request: %w", err)
	}
	for attempt := 0; ; attempt++ {
		data, code, delay, err := c.attempt(ctx, path, body)
		if err != nil {
			return err
		}
		if code >= 200 && code < 300 {
			var envelope struct {
				Error *json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(data, &envelope); err != nil || envelope.Error != nil {
				return errors.New("invalid llm response envelope")
			}
			if err := json.Unmarshal(data, target); err != nil {
				return errors.New("invalid llm response JSON")
			}
			return nil
		}
		if (code != 429 && code < 500) || attempt >= c.cfg.Retries {
			return fmt.Errorf("llm HTTP status %d", code)
		}
		delay = max(delay, time.Second*time.Duration(1<<attempt))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("wait for llm retry: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func (c *Client) attempt(ctx context.Context, path string, body []byte) ([]byte, int, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.cfg.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, 0, errors.New("unable to create llm request")
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// URL и текст транспорта не возвращаем: они могут содержать параметры оператора.
		if ctx.Err() != nil {
			return nil, 0, 0, fmt.Errorf("llm deadline: %w", ctx.Err())
		}
		return nil, 0, 0, errors.New("llm transport failed")
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
	closeErr := resp.Body.Close()
	if readErr != nil || closeErr != nil {
		return nil, 0, 0, errors.New("unable to read llm response")
	}
	if len(data) > 4<<20 {
		return nil, 0, 0, errors.New("llm response too large")
	}
	delay := retryDelay(resp.Header.Get("Retry-After"))
	var envelope struct {
		Error struct {
			Metadata struct {
				RetryAfter json.RawMessage `json:"retry_after"`
			} `json:"metadata"`
			RetryAfter json.RawMessage `json:"retry_after"`
		} `json:"error"`
		RetryAfter json.RawMessage `json:"retry_after"`
	}
	if json.Unmarshal(data, &envelope) == nil {
		for _, raw := range []json.RawMessage{envelope.RetryAfter, envelope.Error.RetryAfter, envelope.Error.Metadata.RetryAfter} {
			value := strings.Trim(string(raw), "\"")
			delay = max(delay, retryDelay(value))
		}
	}
	return data, resp.StatusCode, delay, nil
}

func retryDelay(value string) time.Duration {
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 {
		return time.Duration(min(seconds, 86400) * float64(time.Second))
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(0, time.Until(date))
	}
	return 0
}
