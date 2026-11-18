package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func providerFixture(payment Payment, state string) ProviderPayment {
	result := ProviderPayment{ID: "provider-123", Status: state, Paid: state == "succeeded", Metadata: map[string]string{"payment_id": payment.ID.String()}}
	result.Amount.Value = amountString(payment.AmountKopecks)
	result.Amount.Currency = "RUB"
	result.Confirmation.URL = "https://yookassa.ru/confirm/test"
	return result
}

func testPayment() Payment {
	return Payment{ID: uuid.New(), UserID: uuid.New(), PlanCode: "pro", RequestID: uuid.New(), AmountKopecks: 29900, PeriodDays: 30, Status: "pending", IdempotenceKey: uuid.New(), ReturnURL: "https://example.com/return", CreatedAt: time.Now()}
}

func TestYookassa(t *testing.T) {
	t.Parallel()
	payment := testPayment()
	for _, tc := range []struct {
		name      string
		method    string
		code      int
		body      string
		wantError bool
	}{
		{"create", http.MethodPost, 201, "", false},
		{"get", http.MethodGet, 200, "", false},
		{"unauthorized", http.MethodPost, 401, `{"secret":"must-not-leak"}`, true},
		{"rate_limited", http.MethodGet, 429, `{}`, true},
		{"server_error", http.MethodGet, 500, `{}`, true},
		{"invalid_json", http.MethodGet, 200, `{`, true},
		{"missing_id", http.MethodGet, 200, `{}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				shop, secret, ok := r.BasicAuth()
				if !ok || shop != "shop" || secret != "secret" {
					t.Error("missing basic authentication")
				}
				if r.Method != tc.method {
					t.Errorf("method=%s", r.Method)
				}
				if tc.method == http.MethodPost {
					if r.URL.Path != "/v3/payments" || r.Header.Get("Idempotence-Key") != payment.IdempotenceKey.String() {
						t.Error("missing idempotence key or wrong path")
					}
					var payload struct {
						Capture bool `json:"capture"`
						Amount  struct {
							Value    string `json:"value"`
							Currency string `json:"currency"`
						} `json:"amount"`
						Confirmation struct {
							Type      string `json:"type"`
							ReturnURL string `json:"return_url"`
						} `json:"confirmation"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					if !payload.Capture || payload.Amount.Value != "299.00" || payload.Amount.Currency != "RUB" || payload.Confirmation.Type != "redirect" || payload.Confirmation.ReturnURL != payment.ReturnURL {
						t.Error("wrong checkout payload")
					}
				} else if r.URL.Path != "/v3/payments/provider-123" {
					t.Error("wrong get path")
				}
				w.WriteHeader(tc.code)
				if tc.body != "" {
					if _, err := fmt.Fprint(w, tc.body); err != nil {
						t.Error(err)
					}
					return
				}
				if err := json.NewEncoder(w).Encode(providerFixture(payment, "pending")); err != nil {
					t.Error(err)
				}
			}))
			defer srv.Close()
			client, err := NewYookassa("shop", "secret", srv.URL+"/v3", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			var result ProviderPayment
			if tc.method == http.MethodPost {
				result, err = client.Create(t.Context(), payment)
			} else {
				result, err = client.Get(t.Context(), "provider-123")
			}
			if (err != nil) != tc.wantError {
				t.Fatalf("result=%s err=%v", result.ID, err)
			}
			if err != nil && strings.Contains(err.Error(), "must-not-leak") {
				t.Fatal("provider response leaked")
			}
		})
	}
}

func TestYookassaTimeout(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	client, err := NewYookassa("shop", "secret", srv.URL, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(t.Context(), "payment"); err == nil {
		t.Fatal("timeout was ignored")
	}
}

func TestYookassaDisabled(t *testing.T) {
	t.Parallel()
	client, err := NewYookassa("", "", "", time.Second)
	if client != nil || err != nil {
		t.Fatalf("disabled client=%v err=%v", client, err)
	}
}

// TestYookassaLive создаёт только неподтверждённый платёж тестового магазина, деньги не списывает.
func TestYookassaLive(t *testing.T) {
	shop, secret := os.Getenv("YOOKASSA_SHOP_ID"), os.Getenv("YOOKASSA_SECRET_KEY")
	if shop == "" || secret == "" {
		t.Skip("yookassa test credentials not set")
	}
	apiURL := os.Getenv("YOOKASSA_API_URL")
	if apiURL == "" {
		apiURL = "https://api.yookassa.ru/v3"
	}
	client, err := NewYookassa(shop, secret, apiURL, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	payment := testPayment()
	created, err := client.Create(t.Context(), payment)
	if err != nil {
		t.Fatal(err)
	}
	if !created.Test {
		t.Fatal("expected test shop, payment confirmation must not be performed")
	}
	repeated, err := client.Create(t.Context(), payment)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.ID != created.ID {
		t.Fatal("idempotency created a second payment")
	}
	actual, err := client.Get(context.WithoutCancel(t.Context()), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	payment.ProviderID = &created.ID
	if err := validatePayment(payment, actual); err != nil {
		t.Fatal(err)
	}
	if actual.Status != "pending" || actual.Confirmation.URL == "" {
		t.Fatal("expected pending redirect payment")
	}
	t.Log("test shop: payment created, idempotent retry and GET verified; confirmation not performed")
}
