package submission

import (
	"context"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/autocheck-team/backend/internal/auth"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

// snapshotRepository позволяет проверить настоящий HTTP/gRPC-поток без PostgreSQL.
type snapshotRepository struct {
	submissions
	mu  sync.Mutex
	sub Submission
	err error
}

func (r *snapshotRepository) Get(context.Context, uuid.UUID) (Submission, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sub, r.err
}

func (r *snapshotRepository) set(state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sub.Status = state
	if state == "done" || state == "failed" {
		verdict := "OK"
		if state == "failed" {
			verdict = "SE"
		}
		r.sub.Verdict = &verdict
	}
}

func (*snapshotRepository) TestRuns(context.Context, uuid.UUID) ([]TestRun, error) {
	return []TestRun{{Ordinal: 1, Verdict: "TL", TimeMS: 1001}}, nil
}

type readyDB struct{}

func (readyDB) Ping(context.Context) error { return nil }

func TestSubmissionStreamHTTP(t *testing.T) {
	for _, terminal := range []string{"done", "failed"} {
		t.Run(terminal, func(t *testing.T) {
			// arrange
			owner := auth.Principal{ID: uuid.New(), Roles: []auth.Role{auth.RoleStudent}}
			repo := &snapshotRepository{sub: Submission{
				ID: uuid.New(), UserID: owner.ID,
				LanguageID: "go1.27", Status: "queued",
			}}
			events := &Events{subscribers: make(map[uuid.UUID]map[chan struct{}]struct{})}
			svc := NewService(repo, nil, events)
			base, client := serveGateway(t, svc, owner, readyDB{})
			url := base + "/v1/submissions/" + repo.sub.ID.String() + ":watch"
			// act/assert
			scanner, _ := watchHTTP(t, client, url)
			nextState(t, scanner, v1.SubmissionStatus_SUBMISSION_STATUS_QUEUED)
			// Дубликат сигнала не создаёт дубликат снимка.
			events.signal(repo.sub.ID)
			repo.set("compiling")
			events.signal(repo.sub.ID)
			nextState(t, scanner, v1.SubmissionStatus_SUBMISSION_STATUS_COMPILING)
			repo.set(terminal)
			events.signal(repo.sub.ID)
			want := v1.SubmissionStatus_SUBMISSION_STATUS_DONE
			if terminal == "failed" {
				want = v1.SubmissionStatus_SUBMISSION_STATUS_FAILED
			}
			nextState(t, scanner, want)
			if scanner.Scan() || scanner.Err() != nil {
				t.Fatalf("stream EOF: %v", scanner.Err())
			}
			scanner, _ = watchHTTP(t, client, url)
			nextState(t, scanner, want)
			if scanner.Scan() || scanner.Err() != nil {
				t.Fatalf("terminal stream EOF: %v", scanner.Err())
			}
		})
	}
}

func TestSubmissionStreamCancelHTTP(t *testing.T) {
	owner := auth.Principal{ID: uuid.New()}
	repo := &snapshotRepository{sub: Submission{ID: uuid.New(), UserID: owner.ID, Status: "queued"}}
	events := &Events{subscribers: make(map[uuid.UUID]map[chan struct{}]struct{})}
	base, client := serveGateway(t, NewService(repo, nil, events), owner, readyDB{})
	scanner, body := watchHTTP(t, client, base+"/v1/submissions/"+repo.sub.ID.String()+":watch")
	nextState(t, scanner, v1.SubmissionStatus_SUBMISSION_STATUS_QUEUED)
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		events.mu.Lock()
		defer events.mu.Unlock()
		return len(events.subscribers) == 0
	})
}

func TestSubmissionEndpointsHTTP(t *testing.T) {
	owner := auth.Principal{ID: uuid.New(), Roles: []auth.Role{auth.RoleStudent}}
	repo := &snapshotRepository{sub: Submission{ID: uuid.New(), UserID: owner.ID, Status: "done", LanguageID: "go1.27"}}
	for _, tc := range []struct {
		name string
		user auth.Principal
		code int
	}{
		{"owner", owner, http.StatusOK},
		{"other_student", auth.Principal{ID: uuid.New()}, http.StatusNotFound},
		{"admin", auth.Principal{ID: uuid.New(), Roles: []auth.Role{auth.RoleAdmin}}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := &Events{subscribers: make(map[uuid.UUID]map[chan struct{}]struct{})}
			base, client := serveGateway(t, NewService(repo, nil, events), tc.user, readyDB{})
			for _, suffix := range []string{":watch", "/report"} {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
					base+"/v1/submissions/"+repo.sub.ID.String()+suffix, http.NoBody)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer test")
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				if err := resp.Body.Close(); err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != tc.code {
					t.Fatalf("%s status=%d want=%d", suffix, resp.StatusCode, tc.code)
				}
				if suffix == "/report" && tc.code == http.StatusOK {
					var report v1.GetSubmissionReportResponse
					if err := protojson.Unmarshal(data, &report); err != nil {
						t.Fatal(err)
					}
					if report.GetLanguageId() != "go1.27" || len(report.GetTestRuns()) != 1 || !report.GetTestRuns()[0].GetIsTimeLimitExceeded() {
						t.Fatalf("report=%v", &report)
					}
				}
			}
		})
	}
	base, client := serveGateway(t, NewService(repo, nil, &Events{subscribers: make(map[uuid.UUID]map[chan struct{}]struct{})}), owner, readyDB{})
	for _, tc := range []struct {
		id, token string
		code      int
	}{
		{"invalid", "test", http.StatusBadRequest},
		{repo.sub.ID.String(), "", http.StatusUnauthorized},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/v1/submissions/"+tc.id+":watch", http.NoBody)
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
		if resp.StatusCode != tc.code {
			t.Fatalf("status=%d want=%d", resp.StatusCode, tc.code)
		}
	}
}
