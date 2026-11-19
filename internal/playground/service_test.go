package playground

import (
	"strings"
	"testing"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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

func validCreate() *v1.CreateProjectRequest {
	return &v1.CreateProjectRequest{
		Title: "Мини-проект", LanguageId: "python3.14",
		Files: []*v1.ProjectFile{{Name: "main.py", Content: "print('hello')"}}, ClientRequestId: uuid.NewString(),
	}
}

func wantCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("error=%v want=%s", err, code)
	}
}

func TestFileValidation(t *testing.T) {
	svc := New(nil, testValidator(t))
	ctx := auth.WithPrincipal(t.Context(), auth.Principal{ID: uuid.New()})
	for _, test := range []struct {
		name   string
		change func(*v1.CreateProjectRequest)
	}{
		{"blank_title", func(r *v1.CreateProjectRequest) { r.Title = " \t\n" }},
		{"long_title", func(r *v1.CreateProjectRequest) { r.Title = strings.Repeat("я", 201) }},
		{"no_language", func(r *v1.CreateProjectRequest) { r.LanguageId = "" }},
		{"no_request_id", func(r *v1.CreateProjectRequest) { r.ClientRequestId = "" }},
		{"bad_request_id", func(r *v1.CreateProjectRequest) { r.ClientRequestId = "invalid" }},
		{"no_files", func(r *v1.CreateProjectRequest) { r.Files = nil }},
		{"nil_file", func(r *v1.CreateProjectRequest) { r.Files = []*v1.ProjectFile{nil} }},
		{"duplicate", func(r *v1.CreateProjectRequest) { r.Files = append(r.Files, r.Files[0]) }},
		{"parent_path", func(r *v1.CreateProjectRequest) { r.Files[0].Name = "../main.py" }},
		{"absolute", func(r *v1.CreateProjectRequest) { r.Files[0].Name = "/main.py" }},
		{"windows_path", func(r *v1.CreateProjectRequest) { r.Files[0].Name = `C:\main.py` }},
		{"nested", func(r *v1.CreateProjectRequest) { r.Files[0].Name = "src/main.py" }},
		{"dot", func(r *v1.CreateProjectRequest) { r.Files[0].Name = "." }},
		{"nul_name", func(r *v1.CreateProjectRequest) { r.Files[0].Name = "main\x00.py" }},
		{"invalid_utf8", func(r *v1.CreateProjectRequest) { r.Files[0].Content = string([]byte{255}) }},
		{"nul_content", func(r *v1.CreateProjectRequest) { r.Files[0].Content = "\x00" }},
		{"nul_title", func(r *v1.CreateProjectRequest) { r.Title = "title\x00" }},
		{"large_file", func(r *v1.CreateProjectRequest) { r.Files[0].Content = strings.Repeat("я", (128<<10)+1) }},
		{"large_total", func(r *v1.CreateProjectRequest) {
			r.Files[0].Content = strings.Repeat("x", 200<<10)
			r.Files = append(r.Files, &v1.ProjectFile{Name: "extra", Content: strings.Repeat("x", 60<<10)})
		}},
		{"too_many", func(r *v1.CreateProjectRequest) { r.Files = make([]*v1.ProjectFile, 21) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			// arrange
			request := validCreate()
			test.change(request)
			// act
			_, err := svc.CreateProject(ctx, request)
			// assert
			wantCode(t, err, codes.InvalidArgument)
		})
	}
	if err := validateFiles([]File{{Name: "main.py", Content: strings.Repeat("я", 128<<10)}}, "main.py"); err != nil {
		t.Fatal(err)
	}
}

func TestAuthentication(t *testing.T) {
	svc := New(nil, testValidator(t))
	_, err := svc.CreateProject(t.Context(), validCreate())
	wantCode(t, err, codes.Unauthenticated)
	_, err = svc.GetProject(t.Context(), &v1.GetProjectRequest{})
	wantCode(t, err, codes.Unauthenticated)
	_, err = svc.ListProjects(t.Context(), &v1.ListProjectsRequest{})
	wantCode(t, err, codes.Unauthenticated)
	_, err = svc.UpdateProject(t.Context(), &v1.UpdateProjectRequest{})
	wantCode(t, err, codes.Unauthenticated)
	_, err = svc.DeleteProject(t.Context(), &v1.DeleteProjectRequest{})
	wantCode(t, err, codes.Unauthenticated)
	_, err = svc.RunProject(t.Context(), &v1.RunProjectRequest{})
	wantCode(t, err, codes.Unauthenticated)
	_, err = svc.GetProjectRun(t.Context(), &v1.GetProjectRunRequest{})
	wantCode(t, err, codes.Unauthenticated)
}

func (f *fixture) create(t *testing.T, user auth.Principal) *v1.Project {
	t.Helper()
	response, err := f.svc.CreateProject(auth.WithPrincipal(t.Context(), user), validCreate())
	if err != nil {
		t.Fatal(err)
	}
	return response.Project
}

func TestCRUDAndAccess(t *testing.T) {
	f := setup(t)
	first := f.create(t, f.student)
	second := f.create(t, f.student)
	f.create(t, f.other)
	owner := auth.WithPrincipal(t.Context(), f.student)
	for _, principal := range []auth.Principal{f.student, f.other, f.author, f.admin} {
		ctx := auth.WithPrincipal(t.Context(), principal)
		readCode := codes.NotFound
		if principal.ID == f.student.ID || principal.ID == f.admin.ID {
			readCode = codes.OK
		}
		response, err := f.svc.GetProject(ctx, &v1.GetProjectRequest{Id: first.Id})
		wantCode(t, err, readCode)
		if err == nil && response.Project.Files[0].Content != "print('hello')" {
			t.Fatal("files lost")
		}
		_, err = f.svc.UpdateProject(ctx, &v1.UpdateProjectRequest{Id: first.Id, Title: "new", LanguageId: "python3.14", Files: first.Files})
		writeCode := codes.NotFound
		if principal.ID == f.student.ID {
			writeCode = codes.OK
		}
		wantCode(t, err, writeCode)
		if principal.ID != f.student.ID {
			_, err = f.svc.DeleteProject(ctx, &v1.DeleteProjectRequest{Id: first.Id})
			wantCode(t, err, codes.NotFound)
			_, err = f.svc.RunProject(ctx, &v1.RunProjectRequest{Id: first.Id, ClientRequestId: uuid.NewString()})
			wantCode(t, err, codes.NotFound)
		}
	}
	page, err := f.svc.ListProjects(owner, &v1.ListProjectsRequest{PageSize: 1})
	if err != nil || len(page.Projects) != 1 || page.Projects[0].Id != second.Id || page.NextPageToken == "" {
		t.Fatalf("page=%v err=%v", page, err)
	}
	next, err := f.svc.ListProjects(owner, &v1.ListProjectsRequest{PageSize: 1, PageToken: page.NextPageToken})
	if err != nil || len(next.Projects) != 1 || next.Projects[0].Id != first.Id || next.NextPageToken != "" {
		t.Fatalf("next=%v err=%v", next, err)
	}
	for _, token := range []string{"!", "YQ", "AAAAAAAAAAAAAAAAAAAAAA"} {
		_, err := f.svc.ListProjects(owner, &v1.ListProjectsRequest{PageToken: token})
		wantCode(t, err, codes.InvalidArgument)
	}
	adminPage, err := f.svc.ListProjects(auth.WithPrincipal(t.Context(), f.admin), &v1.ListProjectsRequest{})
	if err != nil || len(adminPage.Projects) != 0 {
		t.Fatal("admin list must contain only own projects", err)
	}
	for range 2 {
		_, err = f.svc.DeleteProject(owner, &v1.DeleteProjectRequest{Id: first.Id})
		wantCode(t, err, codes.OK)
	}
	_, err = f.svc.GetProject(owner, &v1.GetProjectRequest{Id: first.Id})
	wantCode(t, err, codes.NotFound)
	if first.CreateTime.AsTime().IsZero() {
		t.Fatal("missing timestamps")
	}
}

func TestLanguageAndSource(t *testing.T) {
	f := setup(t)
	ctx := auth.WithPrincipal(t.Context(), f.student)
	for _, language := range []string{"missing", "cpp20-gcc15"} {
		request := validCreate()
		request.LanguageId = language
		_, err := f.svc.CreateProject(ctx, request)
		wantCode(t, err, codes.InvalidArgument)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE languages SET is_active=false WHERE id='python3.14'`); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.CreateProject(ctx, validCreate())
	wantCode(t, err, codes.InvalidArgument)
}
