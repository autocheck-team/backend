package monitoring

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Run обслуживает только /metrics на отдельном слушателе и ожидает его остановки.
func (m *Metrics) Run(ctx context.Context, addr string, shutdownTimeout time.Duration) error {
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen metrics: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			done <- errors.Join(fmt.Errorf("shutdown metrics: %w", err), server.Close())
			return
		}
		done <- nil
	}()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return <-done
	}
	// Ошибка Serve тоже завершает горутину остановки.
	cancelRun()
	return errors.Join(fmt.Errorf("serve metrics: %w", err), <-done)
}

// Handler возвращает экспортер для проверок и отдельного metrics-слушателя.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
