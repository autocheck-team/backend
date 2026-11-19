package material

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/autocheck-team/backend/internal/auth"
	"github.com/autocheck-team/backend/internal/storage"
	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

type objectStub struct {
	mu   sync.Mutex
	data map[string][]byte
	gets int
}

func (o *objectStub) PresignPut(_ context.Context, key string, _ time.Duration) (string, error) {
	return "https://s3.test/put/" + key, nil
}

func (o *objectStub) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.gets++
	return "https://s3.test/get/" + key, nil
}

func (o *objectStub) Get(_ context.Context, key string) (io.ReadCloser, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	data, ok := o.data[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (o *objectStub) Put(_ context.Context, key string, reader io.Reader, _ int64, _ string) error {
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.data[key] = data
	return nil
}

func (o *objectStub) DeletePrefix(_ context.Context, prefix string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for key := range o.data {
		if strings.HasPrefix(key, prefix) {
			delete(o.data, key)
		}
	}
	return nil
}

func (o *objectStub) upload(key string, data []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.data[key] = data
}

func TestServiceAccess(t *testing.T) {
	f := setup(t)
	published := f.create(t, true, 0)
	draft := f.create(t, false, 1)
	f.subscribe(t, f.other, "active", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	for _, tc := range []struct {
		name      string
		user      auth.Principal
		locked    bool
		draftCode codes.Code
	}{
		{"free", f.student, true, codes.NotFound},
		{"pro", f.other, false, codes.NotFound},
		{"author_free", f.author, true, codes.NotFound},
		{"admin", f.admin, false, codes.OK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// arrange
			ctx := auth.WithPrincipal(t.Context(), tc.user)
			// act
			result, err := f.svc.GetMaterial(ctx, &v1.GetMaterialRequest{Id: published.ID.String()})
			// assert
			if err != nil || result.GetMaterial().GetIsLocked() != tc.locked || (result.GetMaterial().GetContentMarkdown() == "") != tc.locked {
				t.Fatalf("get=%v err=%v", result, err)
			}
			if _, err := f.svc.GetMaterial(ctx, &v1.GetMaterialRequest{Id: draft.ID.String()}); status.Code(err) != tc.draftCode {
				t.Fatalf("draft=%v", err)
			}
			list, err := f.svc.ListMaterials(ctx, &v1.ListMaterialsRequest{})
			want := 1
			if tc.user.Has(auth.RoleAdmin) {
				want = 2
			}
			if err != nil || len(list.GetMaterials()) != want {
				t.Fatalf("list=%v err=%v", list, err)
			}
			for _, material := range list.GetMaterials() {
				if material.GetContentMarkdown() != "" || len(material.GetAttachments()) != 0 {
					t.Fatal("list exposed paid data")
				}
			}
			_, err = f.svc.CreateMaterial(ctx, &v1.CreateMaterialRequest{RequestId: uuid.NewString(), Material: f.input()})
			if tc.user.Has(auth.RoleAdmin) {
				if err != nil {
					t.Fatal(err)
				}
			} else if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("create=%v", err)
			}
			_, err = f.svc.UpdateMaterial(ctx, &v1.UpdateMaterialRequest{Id: published.ID.String(), Material: f.input()})
			if tc.user.Has(auth.RoleAdmin) {
				if err != nil {
					t.Fatal(err)
				}
			} else if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("update=%v", err)
			}
		})
	}
	for _, tc := range []struct {
		name, state string
		start, end  time.Time
	}{
		{"expired", "active", time.Now().Add(-2 * time.Hour), time.Now().Add(-time.Hour)},
		{"canceled", "canceled", time.Now().Add(-time.Hour), time.Now().Add(time.Hour)},
		{"future", "active", time.Now().Add(time.Hour), time.Now().Add(2 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.subscribe(t, f.other, tc.state, tc.start, tc.end)
			result, err := f.svc.GetMaterial(auth.WithPrincipal(t.Context(), f.other), &v1.GetMaterialRequest{Id: published.ID.String()})
			if err != nil || !result.GetMaterial().GetIsLocked() || result.GetMaterial().GetContentMarkdown() != "" {
				t.Fatalf("access=%v err=%v", result, err)
			}
		})
	}
	if _, err := f.svc.GetMaterial(t.Context(), &v1.GetMaterialRequest{Id: published.ID.String()}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("anonymous=%v", err)
	}
}

func TestAttachmentWorkflow(t *testing.T) {
	f := setup(t)
	material := f.create(t, true, 0)
	ctx := auth.WithPrincipal(t.Context(), f.admin)
	request := &v1.CreateMaterialAttachmentRequest{MaterialId: material.ID.String(), Filename: "lesson.pdf", RequestId: uuid.NewString()}
	created, err := f.svc.CreateMaterialAttachment(ctx, request)
	if err != nil || created.GetUploadUrl() == "" || created.GetMaxSizeBytes() != 1024 {
		t.Fatalf("upload=%v err=%v", created, err)
	}
	attachmentID := uuid.MustParse(created.GetAttachment().GetId())
	confirm := &v1.ConfirmMaterialAttachmentRequest{MaterialId: material.ID.String(), Id: attachmentID.String()}
	if _, err := f.svc.ConfirmMaterialAttachment(ctx, confirm); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("missing=%v", err)
	}
	key := attachmentPrefix(material.ID, attachmentID) + "upload"
	for _, data := range [][]byte{nil, bytes.Repeat([]byte("x"), 1025), []byte("<svg>unsafe</svg>")} {
		f.objects.upload(key, data)
		if _, err := f.svc.ConfirmMaterialAttachment(ctx, confirm); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("invalid upload=%v", err)
		}
	}
	data := []byte("%PDF-1.7\nlesson")
	f.objects.upload(key, data)
	confirmed, err := f.svc.ConfirmMaterialAttachment(ctx, confirm)
	if err != nil || !confirmed.GetAttachment().GetIsReady() || confirmed.GetAttachment().GetSizeBytes() != int64(len(data)) {
		t.Fatalf("confirm=%v err=%v", confirmed, err)
	}
	f.objects.upload(key, []byte("changed after confirm"))
	stored, err := f.repo.Attachment(t.Context(), material.ID, attachmentID)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := f.objects.Get(t.Context(), stored.ObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frozen, data) {
		t.Fatal("PUT overwrote confirmed content")
	}
	repeated, err := f.svc.CreateMaterialAttachment(ctx, request)
	if err != nil || repeated.GetUploadUrl() != "" || repeated.GetAttachment().GetId() != attachmentID.String() {
		t.Fatalf("replay=%v err=%v", repeated, err)
	}
	if _, err := f.svc.ConfirmMaterialAttachment(ctx, confirm); err != nil {
		t.Fatal(err)
	}
	for _, principal := range []auth.Principal{f.student, f.author} {
		userCtx := auth.WithPrincipal(t.Context(), principal)
		if _, err := f.svc.CreateMaterialAttachment(userCtx, request); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("upload permission=%v", err)
		}
		if _, err := f.svc.ConfirmMaterialAttachment(userCtx, confirm); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("confirm permission=%v", err)
		}
		result, err := f.svc.GetMaterial(userCtx, &v1.GetMaterialRequest{Id: material.ID.String()})
		if err != nil || len(result.GetMaterial().GetAttachments()) != 0 || f.objects.gets != 0 {
			t.Fatalf("locked attachment leaked: %v err=%v", result, err)
		}
	}
	f.subscribe(t, f.other, "active", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	result, err := f.svc.GetMaterial(auth.WithPrincipal(t.Context(), f.other), &v1.GetMaterialRequest{Id: material.ID.String()})
	if err != nil || len(result.GetMaterial().GetAttachments()) != 1 || result.GetMaterial().GetAttachments()[0].GetDownloadUrl() == "" {
		t.Fatalf("pro attachment=%v err=%v", result, err)
	}
	deletion := &v1.DeleteMaterialAttachmentRequest{MaterialId: material.ID.String(), Id: attachmentID.String()}
	if _, err := f.svc.DeleteMaterialAttachment(auth.WithPrincipal(t.Context(), f.student), deletion); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("delete permission=%v", err)
	}
	for range 2 {
		if _, err := f.svc.DeleteMaterialAttachment(ctx, deletion); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.objects.data) != 0 {
		t.Fatal("files remain after delete")
	}
}

func TestValidationAndDelete(t *testing.T) {
	f := setup(t)
	ctx := auth.WithPrincipal(t.Context(), f.admin)
	for _, tc := range []struct {
		name   string
		change func(*v1.MaterialInput)
	}{
		{"blank_title", func(input *v1.MaterialInput) { input.Title = " \n" }},
		{"blank_content", func(input *v1.MaterialInput) { input.ContentMarkdown = "\t" }},
		{"no_problems", func(input *v1.MaterialInput) { input.ProblemIds = nil }},
		{"duplicate_problems", func(input *v1.MaterialInput) { input.ProblemIds = append(input.ProblemIds, input.ProblemIds[0]) }},
		{"unknown_problem", func(input *v1.MaterialInput) { input.ProblemIds = []string{uuid.NewString()} }},
		{"invalid_problem", func(input *v1.MaterialInput) { input.ProblemIds = []string{"bad"} }},
		{"negative_ordinal", func(input *v1.MaterialInput) { input.Ordinal = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := f.input()
			tc.change(input)
			if _, err := f.svc.CreateMaterial(ctx, &v1.CreateMaterialRequest{Material: input, RequestId: uuid.NewString()}); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("validation=%v", err)
			}
		})
	}
	material := f.create(t, true, 0)
	for _, name := range []string{"../bad.pdf", "\\bad.pdf", "\n.pdf", " ", ".."} {
		if _, err := f.svc.CreateMaterialAttachment(ctx, &v1.CreateMaterialAttachmentRequest{MaterialId: material.ID.String(), Filename: name, RequestId: uuid.NewString()}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("filename=%q err=%v", name, err)
		}
	}
	req := &v1.DeleteMaterialRequest{Id: material.ID.String()}
	if _, err := f.svc.DeleteMaterial(auth.WithPrincipal(t.Context(), f.author), req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("delete=%v", err)
	}
	for range 2 {
		if _, err := f.svc.DeleteMaterial(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
}
