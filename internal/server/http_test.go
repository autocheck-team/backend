package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

func TestHTTPEndpoints(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gateway := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	tests := []struct {
		name string
		db   Pinger
		path string
		want int
	}{
		{"healthz_ignores_db", fakeDB{err: errors.New("down")}, "/healthz", http.StatusOK},
		{"readyz_db_up", fakeDB{}, "/readyz", http.StatusOK},
		{"readyz_db_down", fakeDB{err: errors.New("down")}, "/readyz", http.StatusServiceUnavailable},
		{"openapi", fakeDB{}, "/openapi.yaml", http.StatusOK},
		{"other_paths_go_to_gateway", fakeDB{}, "/v1/me", http.StatusTeapot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil)
			newHTTPMux(log, tt.db, gateway).ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestCORS(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := cors([]string{"http://localhost:5173"}, next)

	t.Run("preflight_allowed_origin", func(t *testing.T) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, "/v1/me", nil)
		req.Header.Set("Origin", "http://localhost:5173")
		req.Header.Set("Access-Control-Request-Method", "GET")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
			t.Fatalf("code=%d allow-origin=%q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
		}
	})
	t.Run("foreign_origin_gets_no_cors_headers", func(t *testing.T) {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/me", nil)
		req.Header.Set("Origin", "http://evil.example")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("unexpected Access-Control-Allow-Origin")
		}
	})
}
