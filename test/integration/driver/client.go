// Package driver предоставляет REST-клиент для сквозных и нагрузочных тестов.
package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

// Client обращается только к сетевому REST-шлюзу; keep-alive выключен для изоляции тестов.
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// HTTPError сохраняет безопасный ответ шлюза без заголовков и токенов.
type HTTPError struct {
	Status  int
	Message string
}

// Error возвращает HTTP-код и публичное сообщение сервера.
func (e *HTTPError) Error() string { return fmt.Sprintf("gateway HTTP %d: %s", e.Status, e.Message) }

// New создаёт независимый HTTP-транспорт с ограничением времени запроса.
func New(base, token string) *Client {
	return &Client{
		Base: strings.TrimRight(base, "/"), Token: token,
		HTTP: &http.Client{Timeout: 3 * time.Minute, Transport: &http.Transport{DisableKeepAlives: true}},
	}
}

// Call сериализует protobuf как REST JSON и проверяет ответ шлюза.
func (c *Client) Call(parent context.Context, method, path string, input, output proto.Message) (err error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	var body []byte
	if input != nil {
		body, err = protojson.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode REST request: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create REST request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("call REST gateway: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if readErr != nil {
		return fmt.Errorf("read REST response: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		var failure struct {
			Message string `json:"message"`
		}
		if decodeErr := json.Unmarshal(data, &failure); decodeErr != nil {
			return &HTTPError{Status: resp.StatusCode}
		}
		return &HTTPError{Status: resp.StatusCode, Message: failure.Message}
	}
	if output != nil {
		if err := protojson.Unmarshal(data, output); err != nil {
			return fmt.Errorf("decode REST response: %w", err)
		}
	}
	return nil
}

// Login получает настоящий Keycloak access token через autocheck-cli.
func Login(ctx context.Context, issuer, username, password string) (string, error) {
	return login(ctx, issuer, "autocheck-cli", username, password)
}

func login(parent context.Context, issuer, clientID, username, password string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	form := url.Values{"grant_type": {"password"}, "client_id": {clientID}, "username": {username}, "password": {password}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, issuer+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", errors.New("keycloak token transport failed")
	}
	var result struct {
		AccessToken string `json:"access_token"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result)
	closeErr := resp.Body.Close()
	if resp.StatusCode != http.StatusOK || decodeErr != nil || closeErr != nil || result.AccessToken == "" {
		return "", fmt.Errorf("keycloak token request failed: HTTP %d", resp.StatusCode)
	}
	return result.AccessToken, nil
}

// Put загружает данные по неизменённой presigned-ссылке настоящего S3.
func (c *Client) Put(parent context.Context, signedURL, text string) error {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, signedURL, strings.NewReader(text))
	if err != nil {
		return errors.New("invalid S3 upload URL")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return errors.New("S3 upload transport failed")
	}
	closeErr := resp.Body.Close()
	if closeErr != nil {
		return fmt.Errorf("close S3 response: %w", closeErr)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("S3 PUT HTTP %d", resp.StatusCode)
	}
	return nil
}

// Watch читает настоящий NDJSON-поток до DONE; FAILED и преждевременный EOF считаются ошибкой.
func (c *Client) Watch(parent context.Context, id string) (_ *v1.Submission, err error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/v1/submissions/"+id+":watch", nil)
	if err != nil {
		return nil, fmt.Errorf("create watch request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("open watch: %w", err)
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{Status: resp.StatusCode}
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	for {
		var envelope struct {
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := decoder.Decode(&envelope); err != nil {
			return nil, fmt.Errorf("watch ended before DONE: %w", err)
		}
		if len(envelope.Error) > 0 {
			return nil, errors.New("watch returned streaming error")
		}
		var result v1.WatchSubmissionResponse
		if err := protojson.Unmarshal(envelope.Result, &result); err != nil {
			return nil, fmt.Errorf("decode watch: %w", err)
		}
		sub := result.GetSubmission()
		if sub.GetId() != id {
			return nil, errors.New("watch returned another submission")
		}
		switch sub.GetStatus() {
		case v1.SubmissionStatus_SUBMISSION_STATUS_DONE:
			return sub, nil
		case v1.SubmissionStatus_SUBMISSION_STATUS_FAILED:
			return nil, errors.New("submission FAILED with system error")
		}
	}
}
