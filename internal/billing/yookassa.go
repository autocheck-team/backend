// Package billing управляет тарифами, оплатой доступа и квотами пользователей.
package billing

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
)

// ProviderPayment содержит только данные, необходимые для проверки оплаты.
type ProviderPayment struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Paid   bool   `json:"paid"`
	Test   bool   `json:"test"`
	Amount struct {
		Value    string `json:"value"`
		Currency string `json:"currency"`
	} `json:"amount"`
	Confirmation struct {
		URL string `json:"confirmation_url"`
	} `json:"confirmation"`
	Metadata map[string]string `json:"metadata"`
}

// Provider изолирует платёжный API от сценария и позволяет моделировать сетевые сбои.
type Provider interface {
	Create(ctx context.Context, payment Payment) (ProviderPayment, error)
	Get(ctx context.Context, id string) (ProviderPayment, error)
}

// Yookassa вызывает API v3, не включая тела ответов и реквизиты в ошибки.
type Yookassa struct {
	shopID    string
	secretKey string
	apiURL    string
	timeout   time.Duration
	client    *http.Client
}

// NewYookassa возвращает nil без полного набора ключей: бесплатные квоты работают автономно.
func NewYookassa(shopID, secretKey, apiURL string, timeout time.Duration) (*Yookassa, error) {
	if shopID == "" || secretKey == "" {
		return nil, nil
	}
	parsed, err := url.Parse(apiURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, errors.New("invalid yookassa api URL")
	}
	if timeout <= 0 {
		return nil, errors.New("yookassa timeout must be positive")
	}
	return &Yookassa{
		shopID: shopID, secretKey: secretKey, apiURL: strings.TrimRight(apiURL, "/"), timeout: timeout,
		client: &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func amountString(kopecks int64) string { return fmt.Sprintf("%d.%02d", kopecks/100, kopecks%100) }

// Create использует ключ из БД, поэтому повтор после потери ответа не создаёт новую оплату.
func (y *Yookassa) Create(ctx context.Context, payment Payment) (ProviderPayment, error) {
	payload := map[string]any{
		"amount":       map[string]string{"value": amountString(payment.AmountKopecks), "currency": "RUB"},
		"capture":      true,
		"confirmation": map[string]string{"type": "redirect", "return_url": payment.ReturnURL},
		"description":  "Autocheck subscription",
		"metadata":     map[string]string{"payment_id": payment.ID.String()},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return ProviderPayment{}, fmt.Errorf("encode payment: %w", err)
	}
	return y.request(ctx, http.MethodPost, "/payments", payment.IdempotenceKey.String(), body)
}

// Get перечитывает состояние у провайдера, а не доверяет содержимому webhook.
func (y *Yookassa) Get(ctx context.Context, id string) (ProviderPayment, error) {
	if id == "" || strings.ContainsAny(id, "/?#%") {
		return ProviderPayment{}, errors.New("invalid provider payment id")
	}
	return y.request(ctx, http.MethodGet, "/payments/"+url.PathEscape(id), "", nil)
}

func (y *Yookassa) request(ctx context.Context, method, path, key string, body []byte) (result ProviderPayment, err error) {
	ctx, cancel := context.WithTimeout(ctx, y.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, y.apiURL+path, bytes.NewReader(body)) //nolint:gosec // G704: базовый адрес из серверного конфига, ID экранирован, return_url передаётся только в JSON.
	if err != nil {
		return result, errors.New("unable to build yookassa request")
	}
	req.SetBasicAuth(y.shopID, y.secretKey)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotence-Key", key)
	}
	resp, err := y.client.Do(req) //nolint:gosec // G704: URL собирается из проверенного конфига, редиректы отключены.
	if err != nil {
		// url.Error содержит адрес запроса: конфигурация и внешние ответы не должны попадать в логи.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return result, fmt.Errorf("call yookassa: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close yookassa response: %w", closeErr))
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return result, fmt.Errorf("yookassa returned HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return result, fmt.Errorf("decode yookassa response: %w", err)
	}
	if result.ID == "" {
		return result, errors.New("yookassa response has no payment id")
	}
	return result, nil
}
