package feedback

import (
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/autocheck-team/backend/internal/auth"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

func TestServiceTransitions(t *testing.T) {
	f := setup(t)
	ctx := auth.WithPrincipal(t.Context(), f.admin)
	allowed := map[Status]map[Status]bool{
		StatusNew:       {StatusNew: true, StatusAccepted: true, StatusRejected: true},
		StatusAccepted:  {StatusAccepted: true, StatusRejected: true, StatusCompleted: true},
		StatusRejected:  {StatusRejected: true},
		StatusCompleted: {StatusCompleted: true},
	}
	for previous := StatusNew; previous <= StatusCompleted; previous++ {
		for next := StatusNew; next <= StatusCompleted; next++ {
			t.Run(fmt.Sprintf("status_%d_to_%d", previous, next), func(t *testing.T) {
				// arrange
				created := f.create(t, f.student, KindOther)
				if _, err := f.repo.Update(ctx, created.ID, StatusNew, previous, "before"); err != nil {
					t.Fatal(err)
				}
				// act
				_, err := f.svc.UpdateFeedbackRequest(ctx, &v1.UpdateFeedbackRequestRequest{
					Id: created.ID.String(), Status: statusToProto(next), AdminComment: "after",
				})
				// assert
				want := codes.FailedPrecondition
				if allowed[previous][next] {
					want = codes.OK
				}
				if status.Code(err) != want {
					t.Fatalf("err=%v want=%s", err, want)
				}
				saved, err := f.repo.Get(ctx, created.ID)
				if err != nil {
					t.Fatal(err)
				}
				wantStatus, wantComment := previous, "before"
				if want == codes.OK {
					wantStatus, wantComment = next, "after"
				}
				if saved.Status != wantStatus || saved.AdminComment != wantComment {
					t.Fatalf("saved=%+v", saved)
				}
			})
		}
	}
}

func TestServiceOptionalLanguage(t *testing.T) {
	f := setup(t)
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO languages
		(id, name, image, source_file, run_cmd, is_active)
		VALUES ('retired', 'Retired', 'unused', 'main', ARRAY['unused'], false)`); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name     string
		language *string
		code     codes.Code
	}{
		{"absent", nil, codes.OK},
		{"active", proto.String("go1.27"), codes.OK},
		{"inactive", proto.String("retired"), codes.OK},
		{"unknown", proto.String("missing"), codes.InvalidArgument},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := validCreate()
			req.LanguageId = tt.language
			resp, err := f.svc.CreateFeedbackRequest(auth.WithPrincipal(t.Context(), f.student), req)
			if status.Code(err) != tt.code {
				t.Fatalf("err=%v", err)
			}
			if err == nil && (tt.language == nil && resp.GetFeedbackRequest().LanguageId != nil ||
				tt.language != nil && resp.GetFeedbackRequest().GetLanguageId() != *tt.language) {
				t.Fatalf("language presence lost: %v", resp)
			}
		})
	}
}
