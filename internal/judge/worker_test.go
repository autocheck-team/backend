package judge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

type fakeQueue struct {
	finish, fail, release int
	heartbeatErr          error
	finishErr             error
	permanent             bool
}

func (*fakeQueue) Claim(context.Context, string) (Job, error)   { return Job{}, errors.New("unused") }
func (f *fakeQueue) Heartbeat(context.Context, Job) error       { return f.heartbeatErr }
func (f *fakeQueue) Finish(context.Context, Job, Outcome) error { f.finish++; return f.finishErr }
func (f *fakeQueue) Fail(_ context.Context, _ Job, _ error, permanent bool) error {
	f.fail++
	f.permanent = permanent
	return nil
}

func (f *fakeQueue) Release(ctx context.Context, _ Job) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f.release++
	return nil
}

type executorFunc func(context.Context, Job) (Outcome, error)

func (f executorFunc) Run(ctx context.Context, j Job) (Outcome, error) { return f(ctx, j) }

func TestWorkerCancellation(t *testing.T) {
	for _, shutdown := range []bool{true, false} {
		t.Run(map[bool]string{true: "SIGTERM", false: "lease_lost"}[shutdown], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			q := &fakeQueue{heartbeatErr: ErrLeaseLost}
			w := Worker{Queue: q, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), heartbeatInterval: time.Millisecond}
			w.Runner = executorFunc(func(ctx context.Context, _ Job) (Outcome, error) {
				if shutdown {
					cancel()
				}
				<-ctx.Done()
				return Outcome{}, ctx.Err()
			})
			w.process(ctx, Job{Attempts: 1})
			if q.finish != 0 || q.fail != 0 || q.release != map[bool]int{true: 1, false: 0}[shutdown] {
				t.Fatalf("queue=%+v", q)
			}
		})
	}
}

func TestWorkerOutcomes(t *testing.T) {
	for _, cause := range []error{nil, ErrNoTests, errors.New("S3 unavailable")} {
		q := &fakeQueue{}
		w := Worker{Queue: q, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Runner: executorFunc(func(context.Context, Job) (Outcome, error) { return Outcome{Verdict: "OK"}, cause })}
		w.process(t.Context(), Job{Attempts: 1})
		if cause == nil {
			if q.finish != 1 || q.fail != 0 {
				t.Fatal(q)
			}
		} else if q.fail != 1 || q.permanent != errors.Is(cause, ErrNoTests) {
			t.Fatal(q)
		}
	}
}

func TestWorkerFinishFailureRetries(t *testing.T) {
	q := &fakeQueue{finishErr: errors.New("database unavailable")}
	w := Worker{Queue: q, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Runner: executorFunc(func(context.Context, Job) (Outcome, error) { return Outcome{Verdict: "OK"}, nil })}
	w.process(t.Context(), Job{Attempts: 1})
	if q.finish != 1 || q.fail != 1 {
		t.Fatalf("queue=%+v", q)
	}
}
