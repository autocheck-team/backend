package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/autocheck-team/backend/api/openapi"
)

// Pinger проверяет доступность зависимостей (БД) для /readyz.
type Pinger interface {
	Ping(ctx context.Context) error
}

// httpMux — HTTP-эндпоинты поверх шлюза: health-проверки и спецификация API.
// Всё остальное уходит в grpc-gateway.
func (s *Server) httpMux(gateway http.Handler) http.Handler {
	return newHTTPMux(s.log, s.db, gateway)
}

func newHTTPMux(log *slog.Logger, db Pinger, gateway http.Handler) http.Handler {
	mux := http.NewServeMux()
	// Liveness не проверяет зависимости: процесс жив, если отвечает.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// Readiness проверяет БД: без неё сервис не может обслуживать запросы.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			log.WarnContext(ctx, "readiness check failed", "err", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "db unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(openapi.Spec)
	})
	mux.Handle("/", gateway)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
