//go:build integration

package integration

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
	"github.com/autocheck-team/backend/test/integration/driver"
)

func TestPermissionsAndFeedback(t *testing.T) {
	// arrange
	f := setup(t)
	problem := f.problem(t, "python3.14")
	other, _, err := driver.Student(t.Context(), f.keycloak, f.base)
	must(t, err)
	sub := finished(t, f.student, problem, "python3.14", "print(0)")
	// act / assert
	for _, suffix := range []string{"", "/source", "/report", ":watch", "/hints"} {
		code(t, other, http.MethodGet, "/v1/submissions/"+sub.GetId()+suffix, nil, http.StatusNotFound)
	}
	code(t, other, http.MethodPost, "/v1/submissions/"+sub.GetId()+":hint", &v1.CreateSubmissionHintRequest{}, http.StatusNotFound)
	var own v1.GetSubmissionResponse
	call(t, f.author, http.MethodGet, "/v1/submissions/"+sub.GetId(), nil, &own)
	call(t, f.admin, http.MethodGet, "/v1/submissions/"+sub.GetId(), nil, &own)
	var listed v1.ListSubmissionsResponse
	call(t, other, http.MethodGet, "/v1/submissions?problemId="+problem, nil, &listed)
	if len(listed.GetSubmissions()) != 0 {
		t.Fatal("student list leaks another user's submission")
	}
	code(t, driver.New(f.base, ""), http.MethodGet, "/v1/me", nil, http.StatusUnauthorized)
	input := &v1.Problem{Slug: "t14-" + uuid.NewString(), Title: "Черновик", Statement: "Сумма", TimeLimitMs: 1000, MemoryLimitKb: 131072, Checker: v1.Checker_CHECKER_TOKENS, LanguageIds: []string{"python3.14"}}
	code(t, f.student, http.MethodPost, "/v1/problems", input, http.StatusForbidden)
	var draft v1.CreateProblemResponse
	call(t, f.author, http.MethodPost, "/v1/problems", input, &draft)
	code(t, f.student, http.MethodGet, "/v1/problems/"+draft.GetProblem().GetId(), nil, http.StatusNotFound)
	var feedback v1.CreateFeedbackRequestResponse
	request := &v1.CreateFeedbackRequestRequest{Type: v1.FeedbackRequestType_FEEDBACK_REQUEST_TYPE_OTHER, Title: "t14-" + uuid.NewString(), Text: "Добавьте пример", ClientRequestId: uuid.NewString()}
	call(t, f.student, http.MethodPost, "/v1/feedback-requests", request, &feedback)
	var repeated v1.CreateFeedbackRequestResponse
	call(t, f.student, http.MethodPost, "/v1/feedback-requests", request, &repeated)
	id := feedback.GetFeedbackRequest().GetId()
	if repeated.GetFeedbackRequest().GetId() != id {
		t.Fatal("feedback replay created another request")
	}
	path := "/v1/feedback-requests/" + id
	code(t, other, http.MethodGet, path, nil, http.StatusNotFound)
	code(t, f.author, http.MethodGet, path, nil, http.StatusNotFound)
	update := &v1.UpdateFeedbackRequestRequest{Status: v1.FeedbackRequestStatus_FEEDBACK_REQUEST_STATUS_ACCEPTED, AdminComment: "Проверено"}
	code(t, f.student, http.MethodPatch, path, update, http.StatusForbidden)
	var changed v1.UpdateFeedbackRequestResponse
	call(t, f.admin, http.MethodPatch, path, update, &changed)
	if changed.GetFeedbackRequest().GetStatus() != update.GetStatus() {
		t.Fatal("admin status update missing")
	}
	var read v1.GetFeedbackRequestResponse
	call(t, f.student, http.MethodGet, path, nil, &read)
	if read.GetFeedbackRequest().GetAdminComment() != "Проверено" {
		t.Fatal("owner cannot see admin response")
	}
}

func TestBillingCompilationAndCheckout(t *testing.T) {
	// arrange
	f := setup(t)
	problem := f.problem(t, "cpp20-gcc15")
	first, err := f.student.Submit(t.Context(), problem, "cpp20-gcc15", "invalid C++")
	must(t, err)
	// act / assert
	_, err = f.student.Submit(t.Context(), problem, "cpp20-gcc15", "invalid C++")
	var exhausted *driver.HTTPError
	if !errors.As(err, &exhausted) || exhausted.Status != http.StatusTooManyRequests {
		t.Fatalf("free compilation quota: %v", err)
	}
	_, err = f.student.Watch(t.Context(), first.GetId())
	must(t, err)
	f.pro(t, f.student)
	for range 2 {
		result := finished(t, f.student, problem, "cpp20-gcc15", "invalid C++")
		if result.GetVerdict() != v1.Verdict_VERDICT_COMPILATION_ERROR {
			t.Fatal("expected CE after Pro removes compilation limit")
		}
	}
	var subscription v1.GetMySubscriptionResponse
	call(t, f.student, http.MethodGet, "/v1/subscriptions/me", nil, &subscription)
	var checkout v1.CheckoutSubscriptionResponse
	request := &v1.CheckoutSubscriptionRequest{PlanCode: "pro", ReturnUrl: "http://localhost:8080/healthz", RequestId: uuid.NewString()}
	err = f.student.Call(t.Context(), http.MethodPost, "/v1/subscriptions:checkout", request, &checkout)
	if !subscription.GetCheckoutEnabled() {
		var disabled *driver.HTTPError
		if !errors.As(err, &disabled) || disabled.Status != http.StatusServiceUnavailable || !strings.Contains(disabled.Message, "недоступно") {
			t.Fatalf("disabled checkout response: %v", err)
		}
		t.Log("checkout: configured as disabled, HTTP 503 observed")
		return
	}
	must(t, err)
	if checkout.GetPayment().GetId() == "" || checkout.GetConfirmationUrl() == "" {
		t.Fatal("checkout missing payment or confirmation URL")
	}
	var retry v1.CheckoutSubscriptionResponse
	call(t, f.student, http.MethodPost, "/v1/subscriptions:checkout", request, &retry)
	if retry.GetPayment().GetId() != checkout.GetPayment().GetId() {
		t.Fatal("checkout replay created another payment")
	}
	t.Log("checkout: payment created, no payment confirmation performed")
}

func TestBillingMonthlyRuns(t *testing.T) {
	// arrange: исторические прогоны ускоряют достижение настоящего лимита 100 без изменения тарифа.
	f := setup(t)
	problem := f.problem(t, "python3.14")
	f.exec(t, `WITH p AS (
 INSERT INTO problems (author_id,slug,title,statement,time_limit_ms,memory_limit_kb)
 VALUES ($1,$2,'История квоты','Тестовая история',1000,131072) RETURNING id
 ), s AS (
 INSERT INTO submissions (problem_id,user_id,language_id,source_key,source_size,status,verdict)
 SELECT id,$1,'python3.14','fixture',1,'done','OK' FROM p RETURNING id
 ), cases AS (
 INSERT INTO test_cases (problem_id,ordinal,input_key,expected_key)
 SELECT p.id,n,'fixture-input','fixture-expected' FROM p CROSS JOIN generate_series(1,100) n RETURNING id
 ) INSERT INTO test_runs (submission_id,test_case_id,verdict,time_ms,memory_kb)
 SELECT s.id,cases.id,'OK',1,1 FROM s CROSS JOIN cases`, f.studentID, "t14-"+uuid.NewString())
	var snapshot v1.GetMySubscriptionResponse
	call(t, f.student, http.MethodGet, "/v1/subscriptions/me", nil, &snapshot)
	if snapshot.GetTestRunsUsed() != 100 || snapshot.GetTestRunsRemaining() != 0 {
		t.Fatal("fixture did not exhaust real monthly quota")
	}
	// act / assert
	_, err := f.student.Submit(t.Context(), problem, "python3.14", "print(0)")
	var exhausted *driver.HTTPError
	if !errors.As(err, &exhausted) || exhausted.Status != http.StatusTooManyRequests {
		t.Fatalf("free monthly quota: %v", err)
	}
	f.pro(t, f.student)
	result := finished(t, f.student, problem, "python3.14", `print(sum(map(int,input().split())))`)
	if result.GetVerdict() != v1.Verdict_VERDICT_OK {
		t.Fatal("Pro did not remove test run quota")
	}
}

func TestMaterialsAndPlayground(t *testing.T) {
	// arrange
	f := setup(t)
	problem := f.problem(t, "python3.14")
	var created v1.CreateMaterialResponse
	input := &v1.CreateMaterialRequest{RequestId: uuid.NewString(), Material: &v1.MaterialInput{Title: "t14-" + uuid.NewString(), Description: "Доступное описание", ContentMarkdown: "Платное содержимое", ProblemIds: []string{problem}, IsPublished: true}}
	code(t, f.student, http.MethodPost, "/v1/materials", input, http.StatusForbidden)
	call(t, f.admin, http.MethodPost, "/v1/materials", input, &created)
	path := "/v1/materials/" + created.GetMaterial().GetId()
	// act / assert
	var free v1.GetMaterialResponse
	call(t, f.student, http.MethodGet, path, nil, &free)
	if !free.GetMaterial().GetIsLocked() || free.GetMaterial().GetDescription() != "Доступное описание" || free.GetMaterial().GetContentMarkdown() != "" {
		t.Fatal("free material access is incorrect")
	}
	f.pro(t, f.student)
	var paid v1.GetMaterialResponse
	call(t, f.student, http.MethodGet, path, nil, &paid)
	if paid.GetMaterial().GetIsLocked() || paid.GetMaterial().GetContentMarkdown() != "Платное содержимое" {
		t.Fatal("Pro material content missing")
	}
	var project v1.CreateProjectResponse
	call(t, f.student, http.MethodPost, "/v1/projects", &v1.CreateProjectRequest{Title: "t14-" + uuid.NewString(), LanguageId: "python3.14", Files: []*v1.ProjectFile{{Name: "main.py", Content: "print(input().upper())"}}, ClientRequestId: uuid.NewString()}, &project)
	var run v1.RunProjectResponse
	call(t, f.student, http.MethodPost, "/v1/projects/"+project.GetProject().GetId()+":run", &v1.RunProjectRequest{Stdin: "hello\n", ClientRequestId: uuid.NewString()}, &run)
	deadline := time.Now().Add(time.Minute)
	for {
		var state v1.GetProjectRunResponse
		call(t, f.student, http.MethodGet, "/v1/project-runs/"+run.GetRun().GetId(), nil, &state)
		if state.GetRun().GetStatus() == v1.ProjectRunStatus_PROJECT_RUN_STATUS_DONE {
			if state.GetRun().GetVerdict() != "OK" || state.GetRun().GetStdout() != "HELLO\n" {
				t.Fatal("playground output mismatch")
			}
			break
		}
		if state.GetRun().GetStatus() == v1.ProjectRunStatus_PROJECT_RUN_STATUS_FAILED || time.Now().After(deadline) {
			t.Fatal("playground run failed or timed out")
		}
		select {
		case <-t.Context().Done():
			t.Fatal("test canceled")
		case <-time.After(100 * time.Millisecond):
		}
	}
	other, _, err := driver.Student(t.Context(), f.keycloak, f.base)
	must(t, err)
	code(t, other, http.MethodGet, "/v1/projects/"+project.GetProject().GetId(), nil, http.StatusNotFound)
	code(t, other, http.MethodGet, "/v1/project-runs/"+run.GetRun().GetId(), nil, http.StatusNotFound)
}

func TestAIWithDeterministicProvider(t *testing.T) {
	// arrange
	f := setup(t)
	f.pro(t, f.author)
	f.pro(t, f.student)
	problem := f.problem(t, "python3.14")
	var generation v1.CreateTestGenerationResponse
	path := "/v1/problems/" + problem + "/test-generations"
	request := &v1.CreateTestGenerationRequest{RequestId: uuid.NewString(), LanguageId: "python3.14", Source: `print(sum(map(int,input().split())))`, TestCount: 1}
	// act
	call(t, f.author, http.MethodPost, path, request, &generation)
	generationPath := path + "/" + generation.GetGeneration().GetId()
	deadline := time.Now().Add(2 * time.Minute)
	var done v1.GetTestGenerationResponse
	for {
		call(t, f.author, http.MethodGet, generationPath, nil, &done)
		if done.GetGeneration().GetStatus() == v1.TestGenerationStatus_TEST_GENERATION_STATUS_DONE {
			break
		}
		if done.GetGeneration().GetStatus() == v1.TestGenerationStatus_TEST_GENERATION_STATUS_FAILED || time.Now().After(deadline) {
			t.Fatalf("generation failed: %s", done.GetGeneration().GetError())
		}
		select {
		case <-t.Context().Done():
			t.Fatal("test canceled")
		case <-time.After(100 * time.Millisecond):
		}
	}
	// assert
	if len(done.GetGeneration().GetTests()) != 1 {
		t.Fatal("stub generated unexpected candidate count")
	}
	candidate := done.GetGeneration().GetTests()[0]
	if string(candidate.GetInput()) != "2 3\n" || string(candidate.GetExpected()) != "5\n" || candidate.GetRejectionReason() != "" {
		t.Fatal("reference was not executed against stub input")
	}
	var accepted v1.AcceptTestGenerationResponse
	selection := &v1.AcceptTestGenerationRequest{TestIds: []string{candidate.GetId()}}
	call(t, f.author, http.MethodPost, generationPath+":accept", selection, &accepted)
	call(t, f.author, http.MethodPost, generationPath+":accept", selection, &accepted)
	if !accepted.GetGeneration().GetTests()[0].GetIsAccepted() {
		t.Fatal("candidate not accepted")
	}
	var cases v1.ListTestCasesResponse
	call(t, f.author, http.MethodGet, "/v1/problems/"+problem+"/test-cases", nil, &cases)
	found := 0
	for _, test := range cases.GetTestCases() {
		if test.GetId() == candidate.GetId() {
			found++
			if !test.GetIsGenerated() || test.GetIsSample() || !test.GetIsReady() {
				t.Fatal("accepted test is not a ready closed generated test")
			}
		}
	}
	if found != 1 {
		t.Fatal("accept was not idempotent")
	}
	deadline = time.Now().Add(30 * time.Second)
	for {
		var recommendations v1.RecommendProblemsResponse
		call(t, f.student, http.MethodPost, "/v1/problems:recommend", &v1.RecommendProblemsRequest{Goal: "Сумма двух чисел", LanguageId: "python3.14", Limit: 10}, &recommendations)
		hasProblem := false
		for _, item := range recommendations.GetRecommendations() {
			if item.GetProblemId() == problem {
				hasProblem = true
			}
		}
		if !recommendations.GetIsTextFallback() && hasProblem {
			if recommendations.GetExplanation() != "Эти задания помогают потренироваться на небольших примерах." {
				t.Fatal("recommendation did not use deterministic chat response")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stub embeddings index did not become ready")
		}
		select {
		case <-t.Context().Done():
			t.Fatal("test canceled")
		case <-time.After(time.Second):
		}
	}
	sub := finished(t, f.student, problem, "python3.14", "print(0)")
	var hint v1.CreateSubmissionHintResponse
	hintPath := "/v1/submissions/" + sub.GetId() + ":hint"
	call(t, f.student, http.MethodPost, hintPath, &v1.CreateSubmissionHintRequest{}, &hint)
	if hint.GetHint().GetText() != "Какие значения вы прочитали и что выводите для небольшого примера?" || hint.GetHint().GetIsFallback() {
		t.Fatal("hint did not use deterministic provider")
	}
	var retry v1.CreateSubmissionHintResponse
	call(t, f.student, http.MethodPost, hintPath, &v1.CreateSubmissionHintRequest{}, &retry)
	if retry.GetHint().GetId() != hint.GetHint().GetId() {
		t.Fatal("hint replay created another record")
	}
}
