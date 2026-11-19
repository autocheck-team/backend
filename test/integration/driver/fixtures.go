package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

// Student создаёт отдельную учётную запись через административный API тестового Keycloak.
// Рабочий access token выдаётся обычным клиентом autocheck-cli, как у стандартных пользователей.
func Student(parent context.Context, keycloak, base string) (*Client, string, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	token, err := login(ctx, keycloak+"/realms/master", "admin-cli", "admin", "admin")
	if err != nil {
		return nil, "", err
	}
	username := "hprpr14-" + uuid.NewString() + "@autocheck.local"
	payload, err := json.Marshal(map[string]any{
		"username": username, "email": username, "emailVerified": true, "enabled": true,
		"firstName": "Integration", "lastName": "Student",
		"credentials": []any{map[string]any{"type": "password", "value": "student", "temporary": false}},
	})
	if err != nil {
		return nil, "", fmt.Errorf("encode test user: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, keycloak+"/admin/realms/autocheck/users", bytes.NewReader(payload))
	if err != nil {
		return nil, "", fmt.Errorf("create user request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, "", errors.New("keycloak create user transport failed")
	}
	closeErr := resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || closeErr != nil {
		return nil, "", fmt.Errorf("keycloak create user HTTP %d", resp.StatusCode)
	}
	access, err := Login(ctx, keycloak+"/realms/autocheck", username, "student")
	if err != nil {
		return nil, "", err
	}
	client := New(base, access)
	var me v1.GetMeResponse
	if err := client.Call(ctx, http.MethodGet, "/v1/me", nil, &me); err != nil {
		return nil, "", err
	}
	return client, me.GetUser().GetId(), nil
}

// Pro выдаёт подписку только через отдельное подключение к БД одноразового тестового стенда.
func Pro(parent context.Context, databaseURL, userID string) error {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return errors.New("connect test subscription database failed")
	}
	_, writeErr := conn.Exec(ctx, `INSERT INTO subscriptions (user_id,plan_code,status,starts_at,ends_at)
 VALUES ($1,'pro','active',now(),now()+interval '30 days')
 ON CONFLICT (user_id) DO UPDATE SET plan_code='pro',status='active',starts_at=now(),ends_at=now()+interval '30 days',updated_at=now()`, userID)
	closeErr := conn.Close(ctx)
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("grant test subscription: %w", err)
	}
	return nil
}

// Problem загружает два теста через S3 и публикует собственное задание автора.
func (c *Client) Problem(ctx context.Context, languages []string) (string, error) {
	var created v1.CreateProblemResponse
	problem := &v1.Problem{
		Slug: "t14-" + uuid.NewString(), Title: "Сумма двух чисел " + uuid.NewString(), Statement: "Прочитайте два целых числа и выведите их сумму.",
		TimeLimitMs: 1000, MemoryLimitKb: 128 * 1024, Checker: v1.Checker_CHECKER_TOKENS, LanguageIds: languages, AllowAiHints: true,
	}
	if err := c.Call(ctx, http.MethodPost, "/v1/problems", problem, &created); err != nil {
		return "", err
	}
	id := created.GetProblem().GetId()
	for _, test := range []struct {
		input, expected string
		sample          bool
	}{{"2 3\n", "5\n", true}, {"17 25\n", "42\n", false}} {
		var upload v1.CreateTestCaseResponse
		path := "/v1/problems/" + id + "/test-cases"
		if err := c.Call(ctx, http.MethodPost, path, &v1.CreateTestCaseRequest{IsSample: test.sample}, &upload); err != nil {
			return "", err
		}
		if err := c.Put(ctx, upload.GetInputUploadUrl(), test.input); err != nil {
			return "", err
		}
		if err := c.Put(ctx, upload.GetExpectedUploadUrl(), test.expected); err != nil {
			return "", err
		}
		var confirmed v1.ConfirmTestCaseResponse
		if err := c.Call(ctx, http.MethodPost, path+"/"+upload.GetTestCase().GetId()+":confirm", &v1.ConfirmTestCaseRequest{}, &confirmed); err != nil {
			return "", err
		}
		if !confirmed.GetTestCase().GetIsReady() {
			return "", errors.New("S3 test upload not ready")
		}
	}
	var published v1.UpdateProblemResponse
	if err := c.Call(ctx, http.MethodPatch, "/v1/problems/"+id+"?update_mask=is_published", &v1.Problem{IsPublished: true}, &published); err != nil {
		return "", err
	}
	if !published.GetProblem().GetIsPublished() {
		return "", errors.New("problem not published")
	}
	return id, nil
}

// Submit создаёт посылку с уникальным ключом повтора.
func (c *Client) Submit(ctx context.Context, problemID, language, source string) (*v1.Submission, error) {
	var result v1.CreateSubmissionResponse
	err := c.Call(ctx, http.MethodPost, "/v1/problems/"+problemID+"/submissions", &v1.CreateSubmissionRequest{ProblemId: problemID, LanguageId: language, Source: source, ClientRequestId: uuid.NewString()}, &result)
	if err != nil {
		return nil, err
	}
	return result.GetSubmission(), nil
}
