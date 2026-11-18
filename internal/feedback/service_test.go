package feedback

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/autocheck-team/backend/internal/auth"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

func testValidator(t *testing.T) protovalidate.Validator {
	t.Helper()
	validator, err := protovalidate.New()
	if err != nil {
		t.Fatal(err)
	}
	return validator
}

func validCreate() *v1.CreateFeedbackRequestRequest {
	return &v1.CreateFeedbackRequestRequest{
		Type:  v1.FeedbackRequestType_FEEDBACK_REQUEST_TYPE_COMPILER_UPDATE,
		Title: "Обновите Go", Text: "Нужна новая версия компилятора.",
	}
}

func TestCreateValidation(t *testing.T) {
	// arrange
	svc := NewService(nil, testValidator(t))
	ctx := auth.WithPrincipal(t.Context(), auth.Principal{ID: uuid.New()})
	tests := []struct {
		name   string
		change func(*v1.CreateFeedbackRequestRequest)
	}{
		{"unspecified_type", func(r *v1.CreateFeedbackRequestRequest) { r.Type = 0 }},
		{"unknown_type", func(r *v1.CreateFeedbackRequestRequest) { r.Type = 100 }},
		{"empty_title", func(r *v1.CreateFeedbackRequestRequest) { r.Title = "" }},
		{"blank_title", func(r *v1.CreateFeedbackRequestRequest) { r.Title = " \n\t" }},
		{"long_title", func(r *v1.CreateFeedbackRequestRequest) { r.Title = strings.Repeat("я", 201) }},
		{"empty_text", func(r *v1.CreateFeedbackRequestRequest) { r.Text = "" }},
		{"blank_text", func(r *v1.CreateFeedbackRequestRequest) { r.Text = " \t" }},
		{"long_text", func(r *v1.CreateFeedbackRequestRequest) { r.Text = strings.Repeat("я", 10001) }},
		{"empty_language", func(r *v1.CreateFeedbackRequestRequest) { r.LanguageId = proto.String("") }},
		{"long_language", func(r *v1.CreateFeedbackRequestRequest) { r.LanguageId = proto.String(strings.Repeat("x", 101)) }},
		{"invalid_request_id", func(r *v1.CreateFeedbackRequestRequest) { r.ClientRequestId = "not-a-uuid" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := validCreate()
			tt.change(req)
			// act
			_, err := svc.CreateFeedbackRequest(ctx, req)
			// assert
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("code=%s err=%v", status.Code(err), err)
			}
		})
	}
	for _, req := range []*v1.CreateFeedbackRequestRequest{
		validCreate(),
		{
			Type:  v1.FeedbackRequestType_FEEDBACK_REQUEST_TYPE_OTHER,
			Title: strings.Repeat("я", 200), Text: strings.Repeat("я", 10000),
		},
	} {
		if err := svc.validator.Validate(req); err != nil {
			t.Fatal(err)
		}
	}
}

func TestServiceAuthentication(t *testing.T) {
	svc := NewService(nil, testValidator(t))
	for _, call := range []struct {
		name string
		call func(context.Context) error
	}{
		{"create", func(ctx context.Context) error { _, err := svc.CreateFeedbackRequest(ctx, validCreate()); return err }},
		{"list", func(ctx context.Context) error {
			_, err := svc.ListFeedbackRequests(ctx, &v1.ListFeedbackRequestsRequest{})
			return err
		}},
		{"get", func(ctx context.Context) error {
			_, err := svc.GetFeedbackRequest(ctx, &v1.GetFeedbackRequestRequest{})
			return err
		}},
		{"update", func(ctx context.Context) error {
			_, err := svc.UpdateFeedbackRequest(ctx, &v1.UpdateFeedbackRequestRequest{})
			return err
		}},
	} {
		t.Run(call.name, func(t *testing.T) {
			if err := call.call(t.Context()); status.Code(err) != codes.Unauthenticated {
				t.Fatalf("err=%v", err)
			}
		})
	}
	for _, role := range []auth.Role{auth.RoleStudent, auth.RoleAuthor} {
		t.Run(string(role), func(t *testing.T) {
			ctx := auth.WithPrincipal(t.Context(), auth.Principal{ID: uuid.New(), Roles: []auth.Role{role}})
			_, err := svc.UpdateFeedbackRequest(ctx, &v1.UpdateFeedbackRequestRequest{})
			if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestReadAndUpdateValidation(t *testing.T) {
	svc := NewService(nil, testValidator(t))
	ctx := auth.WithPrincipal(t.Context(), auth.Principal{ID: uuid.New(), Roles: []auth.Role{auth.RoleAdmin}})
	for _, req := range []*v1.ListFeedbackRequestsRequest{
		{PageSize: -1},
		{PageSize: 101},
		{Status: 99},
		{Type: 99},
		{PageToken: "!"},
		{PageToken: "YQ"},
		{PageToken: base64.RawURLEncoding.EncodeToString(make([]byte, 16))},
	} {
		if _, err := svc.ListFeedbackRequests(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("list %v: %v", req, err)
		}
	}
	if _, err := svc.GetFeedbackRequest(ctx, &v1.GetFeedbackRequestRequest{Id: "invalid"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("get: %v", err)
	}
	for _, req := range []*v1.UpdateFeedbackRequestRequest{
		{Id: "invalid", Status: 1},
		{Id: uuid.NewString()},
		{Id: uuid.NewString(), Status: 99},
		{Id: uuid.NewString(), Status: 1, AdminComment: strings.Repeat("я", 5001)},
	} {
		if _, err := svc.UpdateFeedbackRequest(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("update %v: %v", req, err)
		}
	}
}

func TestStatusTransitions(t *testing.T) {
	allowed := map[Status]map[Status]bool{
		StatusNew:       {StatusNew: true, StatusAccepted: true, StatusRejected: true},
		StatusAccepted:  {StatusAccepted: true, StatusRejected: true, StatusCompleted: true},
		StatusRejected:  {StatusRejected: true},
		StatusCompleted: {StatusCompleted: true},
	}
	for previous := StatusUnspecified; previous <= StatusCompleted+1; previous++ {
		for next := StatusUnspecified; next <= StatusCompleted+1; next++ {
			if got := canTransition(previous, next); got != allowed[previous][next] {
				t.Errorf("transition %d -> %d = %v", previous, next, got)
			}
		}
	}
}
