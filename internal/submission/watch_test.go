package submission

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/autocheck-team/backend/internal/auth"
	"github.com/autocheck-team/backend/internal/judge"
	"github.com/autocheck-team/backend/internal/server"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

type watchVerifier struct{ principal auth.Principal }

func (v watchVerifier) Verify(context.Context, string) (auth.Principal, error) {
	return v.principal, nil
}

func waitFor(t *testing.T, check func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !check() {
		select {
		case <-ctx.Done():
			t.Fatal("condition did not become true")
		case <-ticker.C:
		}
	}
}

// gatewayFixture поднимает настоящий gRPC + gateway; только проверка токена заменена стабом.
func gatewayFixture(t *testing.T, f *fixture, who auth.Principal) (string, *http.Client, *Events) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := f.pool.Config().ConnConfig.Copy()
	cfg.RuntimeParams["application_name"] = "watch-test-" + uuid.NewString()
	events := NewEvents(cfg, log)
	f.svc = NewService(NewRepository(f.pool), f.objects, events)
	var listeners sync.WaitGroup
	listeners.Go(func() { events.Run(ctx) })
	t.Cleanup(func() {
		cancel()
		listeners.Wait()
	})
	base, client := serveGateway(t, f.svc, who, f.pool)
	waitFor(t, func() bool {
		var ready bool
		err := f.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
 WHERE application_name = $1 AND query = 'LISTEN submission_events' AND state = 'idle')`,
			cfg.RuntimeParams["application_name"]).Scan(&ready)
		if err != nil {
			t.Fatal(err)
		}
		return ready
	})
	return base, client, events
}

func serveGateway(t *testing.T, svc *Service, who auth.Principal, db server.Pinger) (string, *http.Client) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// HTTP-сервер сам открывает слушатель; свободный порт выбираем только для теста.
	lis, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	if err := lis.Close(); err != nil {
		t.Fatal(err)
	}
	validator, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	authn := auth.NewInterceptors(watchVerifier{who}, nil, server.PublicMethods...)
	srv := server.New(server.Config{
		GRPCAddr: "127.0.0.1:0", HTTPAddr: addr,
		ShutdownTimeout: 100 * time.Millisecond,
	}, log, db, authn, validator,
		server.Services{
			User: &v1.UnimplementedUserServiceServer{}, Problem: &v1.UnimplementedProblemServiceServer{},
			Submission: svc,
		})
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	t.Cleanup(func() {
		cancel()
		client.CloseIdleConnections()
		if err := <-done; err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("stop gateway: %v", err)
		}
	})
	base := "http://" + addr
	waitFor(t, func() bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/readyz", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode == http.StatusOK
	})
	return base, client
}

func watchHTTP(t *testing.T, client *http.Client, url string) (*bufio.Scanner, io.ReadCloser) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("watch HTTP status = %d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	return scanner, resp.Body
}

func nextState(t *testing.T, scanner *bufio.Scanner, want v1.SubmissionStatus) {
	t.Helper()
	if !scanner.Scan() {
		t.Fatalf("missing state %v: %v", want, scanner.Err())
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	var response v1.WatchSubmissionResponse
	if err := protojson.Unmarshal(envelope.Result, &response); err != nil {
		t.Fatal(err)
	}
	if response.GetSubmission().GetStatus() != want {
		t.Fatalf("state=%v want=%v", response.GetSubmission().GetStatus(), want)
	}
}

func TestWatchSubmissionGateway(t *testing.T) {
	for _, terminal := range []string{"done", "failed"} {
		t.Run(terminal, func(t *testing.T) {
			f := setup(t)
			sub := f.submit(t, f.student, f.published, "python3.14", "print(1)", "")
			base, client, _ := gatewayFixture(t, f, f.student)
			scanner, _ := watchHTTP(t, client, base+"/v1/submissions/"+sub.GetId()+":watch")
			nextState(t, scanner, v1.SubmissionStatus_SUBMISSION_STATUS_QUEUED)
			// Отдельное соединение воркера пишет транзакцию; LISTEN API получает сигнал.
			repo := judge.NewRepository(f.pool)
			job, err := repo.Claim(t.Context(), "watch-worker")
			if err != nil {
				t.Fatal(err)
			}
			if job.SubmissionID.String() != sub.GetId() {
				t.Fatal("unexpected job")
			}
			if err := repo.Start(t.Context(), job); err != nil {
				t.Fatal(err)
			}
			nextState(t, scanner, v1.SubmissionStatus_SUBMISSION_STATUS_COMPILING)
			if err := repo.Running(t.Context(), job); err != nil {
				t.Fatal(err)
			}
			nextState(t, scanner, v1.SubmissionStatus_SUBMISSION_STATUS_RUNNING)
			want := v1.SubmissionStatus_SUBMISSION_STATUS_DONE
			if terminal == "done" {
				err = repo.Finish(t.Context(), job, judge.Outcome{Verdict: "OK", Score: 1})
			} else {
				err = repo.Fail(t.Context(), job, errors.New("test failure"), true)
				want = v1.SubmissionStatus_SUBMISSION_STATUS_FAILED
			}
			if err != nil {
				t.Fatal(err)
			}
			nextState(t, scanner, want)
			if scanner.Scan() || scanner.Err() != nil {
				t.Fatalf("stream did not close cleanly: %s %v", scanner.Text(), scanner.Err())
			}
			// Финальная посылка сразу возвращается единственным сообщением.
			scanner, _ = watchHTTP(t, client, base+"/v1/submissions/"+sub.GetId()+":watch")
			nextState(t, scanner, want)
			if scanner.Scan() || scanner.Err() != nil {
				t.Fatal("terminal stream did not close")
			}
		})
	}
}

func TestSubmissionStreamValidationAndAccess(t *testing.T) {
	f := setup(t)
	sub := f.submit(t, f.student, f.published, "python3.14", "print(1)", "")
	base, client, _ := gatewayFixture(t, f, f.student2)
	for _, tc := range []struct {
		path, token string
		want        int
	}{
		{"/v1/submissions/invalid:watch", "test", http.StatusBadRequest},
		{"/v1/submissions/" + sub.GetId() + ":watch", "test", http.StatusNotFound},
		{"/v1/submissions/" + sub.GetId() + "/report", "test", http.StatusNotFound},
		{"/v1/submissions/" + sub.GetId() + ":watch", "", http.StatusUnauthorized},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+tc.path, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != tc.want {
			t.Fatalf("path=%s status=%d want=%d", tc.path, resp.StatusCode, tc.want)
		}
	}
}

func TestEventsReconnectAndCommit(t *testing.T) {
	f := setup(t)
	sub := f.submit(t, f.student, f.published, "python3.14", "print(1)", "")
	_, _, events := gatewayFixture(t, f, f.student)
	changed, unsubscribe := events.subscribe(uuid.MustParse(sub.GetId()))
	defer unsubscribe()
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("rollback notification transaction: %v", err)
		}
	})
	if _, err := tx.Exec(t.Context(), `UPDATE submissions SET status='compiling' WHERE id=$1`, sub.GetId()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `SELECT pg_notify('submission_events',$1)`, sub.GetId()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
		t.Fatal("notification delivered before commit")
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(3 * time.Second):
		t.Fatal("committed notification missing")
	}
	var terminated bool
	if err := f.pool.QueryRow(t.Context(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
 WHERE application_name=$1`, events.cfg.RuntimeParams["application_name"]).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate listener: %t %v", terminated, err)
	}
	// Без нового NOTIFY переподключение тоже должно восстановить финальное состояние.
	if _, err := f.pool.Exec(t.Context(), `UPDATE submissions SET status='done',verdict='OK' WHERE id=$1`, sub.GetId()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("reconnect did not resynchronize")
	}
}

func TestEventsSlowSubscriber(t *testing.T) {
	events := &Events{subscribers: make(map[uuid.UUID]map[chan struct{}]struct{})}
	id := uuid.New()
	slow, cancelSlow := events.subscribe(id)
	defer cancelSlow()
	fast, cancelFast := events.subscribe(id)
	defer cancelFast()
	other, cancelOther := events.subscribe(uuid.New())
	defer cancelOther()
	for range 100 {
		events.signal(id)
		select {
		case <-fast:
		default:
			t.Fatal("fast subscriber missed signal")
		}
	}
	if len(slow) != 1 || len(other) != 0 {
		t.Fatal("unexpected coalescing or routing")
	}
	events.signal(uuid.Nil)
	if len(other) != 1 {
		t.Fatal("resynchronization missed subscriber")
	}
}

func TestSubmissionReportGateway(t *testing.T) {
	f := setup(t)
	sub := f.submit(t, f.student, f.published, "python3.14", "print(6)", "")
	for i, sample := range []bool{true, false} {
		id := uuid.New()
		if _, err := f.pool.Exec(t.Context(), `INSERT INTO test_cases
 (id,problem_id,ordinal,input_key,expected_key,is_sample,is_ready) VALUES ($1,$2,$3,'input','expected',$4,true)`,
			id, f.published, i+1, sample); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(t.Context(), `INSERT INTO test_runs
 (submission_id,test_case_id,verdict,time_ms,memory_kb,stdout_key) VALUES ($1,$2,'WA',12,2048,'actual')`, sub.GetId(), id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE submissions SET status='done',verdict='WA',score=0 WHERE id=$1`, sub.GetId()); err != nil {
		t.Fatal(err)
	}
	f.objects.data["input"], f.objects.data["expected"], f.objects.data["actual"] = []byte("2 3\n"), []byte("5\n"), []byte("6\n")
	base, client, _ := gatewayFixture(t, f, f.student)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/v1/submissions/"+sub.GetId()+"/report", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var report v1.GetSubmissionReportResponse
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("report HTTP=%d", resp.StatusCode)
	}
	if err := protojson.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.GetTestRuns()) != 2 || report.GetTestRuns()[0].GetDiff() != "-5\n+6\n" ||
		report.GetTestRuns()[1].GetInput() != nil || report.GetTestRuns()[1].GetExpectedOutput() != nil ||
		report.GetTestRuns()[1].GetActualOutput() != nil || strings.Contains(report.GetTestRuns()[1].GetDiff(), "5") {
		t.Fatalf("report=%v", &report)
	}
}
