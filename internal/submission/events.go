package submission

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Events раздаёт сигналы одного LISTEN-соединения всем локальным подписчикам.
// Буфер хранит признак изменений: состояние всегда перечитывается из БД.
type Events struct {
	cfg         *pgx.ConnConfig
	log         *slog.Logger
	mu          sync.Mutex
	subscribers map[uuid.UUID]map[chan struct{}]struct{}
}

// NewEvents сохраняет конфиг отдельного соединения, не занимая слот пула.
func NewEvents(cfg *pgx.ConnConfig, log *slog.Logger) *Events {
	return &Events{cfg: cfg.Copy(), log: log, subscribers: make(map[uuid.UUID]map[chan struct{}]struct{})}
}

func (e *Events) subscribe(id uuid.UUID) (<-chan struct{}, func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch := make(chan struct{}, 1)
	if e.subscribers[id] == nil {
		e.subscribers[id] = make(map[chan struct{}]struct{})
	}
	e.subscribers[id][ch] = struct{}{}
	return ch, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		delete(e.subscribers[id], ch)
		if len(e.subscribers[id]) == 0 {
			delete(e.subscribers, id)
		}
	}
}

func (e *Events) signal(id uuid.UUID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for subID, subscribers := range e.subscribers {
		if id != uuid.Nil && subID != id {
			continue
		}
		for ch := range subscribers {
			select {
			case ch <- struct{}{}:
			default:
				// Медленный клиент получит последнее состояние, не блокируя остальных.
			}
		}
	}
}

// Run переподключается до отмены ctx; после LISTEN будит всех для восстановления пропусков.
// Вызывающий запускает ровно один Run на процесс и дожидается его остановки.
func (e *Events) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := e.listen(ctx); err != nil && ctx.Err() == nil {
			e.log.WarnContext(ctx, "submission listener disconnected", "err", err)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (e *Events) listen(ctx context.Context) error {
	connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, err := pgx.ConnectConfig(connectCtx, e.cfg.Copy())
	cancel()
	if err != nil {
		return fmt.Errorf("connect submission listener: %w", err)
	}
	defer func() {
		closeCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		if err := conn.Close(closeCtx); err != nil {
			e.log.WarnContext(closeCtx, "close submission listener", "err", err)
		}
	}()
	listenCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	_, err = conn.Exec(listenCtx, `LISTEN submission_events`)
	stop()
	if err != nil {
		return fmt.Errorf("listen submission events: %w", err)
	}
	e.signal(uuid.Nil)
	for ctx.Err() == nil {
		waitCtx, stop := context.WithTimeout(ctx, 30*time.Second)
		notification, err := conn.WaitForNotification(waitCtx)
		stop()
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			pingCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			err = conn.Ping(pingCtx)
			stop()
			if err == nil {
				continue
			}
		}
		if err != nil {
			return fmt.Errorf("wait submission event: %w", err)
		}
		if id, err := uuid.Parse(notification.Payload); err == nil {
			e.signal(id)
		}
	}
	return nil
}
