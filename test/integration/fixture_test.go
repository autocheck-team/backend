//go:build integration

// Package integration проверяет изолированный стенд через REST, настоящий Keycloak, S3 и Docker.
package integration

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
	"github.com/autocheck-team/backend/test/integration/driver"
)

type fixture struct {
	author, admin, student              *driver.Client
	studentID, database, keycloak, base string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{base: os.Getenv("E2E_API_URL"), keycloak: os.Getenv("E2E_KEYCLOAK_URL"), database: os.Getenv("E2E_DATABASE_URL")}
	if f.base == "" || f.keycloak == "" || f.database == "" {
		t.Fatal("run make test-integration: E2E_API_URL, E2E_KEYCLOAK_URL and E2E_DATABASE_URL are required")
	}
	for _, role := range []string{"student", "author", "admin"} {
		token, err := driver.Login(t.Context(), f.keycloak+"/realms/autocheck", role+"@autocheck.local", role)
		must(t, err)
		client := driver.New(f.base, token)
		var me v1.GetMeResponse
		call(t, client, http.MethodGet, "/v1/me", nil, &me)
		expected := map[string]v1.Role{"student": v1.Role_ROLE_STUDENT, "author": v1.Role_ROLE_AUTHOR, "admin": v1.Role_ROLE_ADMIN}[role]
		found := false
		for _, actual := range me.GetUser().GetRoles() {
			if actual == expected {
				found = true
			}
		}
		if !found {
			t.Fatalf("default %s account missing role", role)
		}
		switch role {
		case "author":
			f.author = client
		case "admin":
			f.admin = client
		}
	}
	var err error
	f.student, f.studentID, err = driver.Student(t.Context(), f.keycloak, f.base)
	must(t, err)
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func call(t *testing.T, c *driver.Client, method, path string, input, output proto.Message) {
	t.Helper()
	must(t, c.Call(t.Context(), method, path, input, output))
}

func code(t *testing.T, c *driver.Client, method, path string, input proto.Message, want int) {
	t.Helper()
	err := c.Call(t.Context(), method, path, input, nil)
	var failure *driver.HTTPError
	if !errors.As(err, &failure) || failure.Status != want {
		t.Fatalf("%s %s: got %v, want HTTP %d", method, path, err, want)
	}
}

func (f *fixture) pro(t *testing.T, c *driver.Client) {
	t.Helper()
	var me v1.GetMeResponse
	call(t, c, http.MethodGet, "/v1/me", nil, &me)
	must(t, driver.Pro(t.Context(), f.database, me.GetUser().GetId()))
	var subscription v1.GetMySubscriptionResponse
	call(t, c, http.MethodGet, "/v1/subscriptions/me", nil, &subscription)
	if subscription.GetPlan().GetCode() != "pro" {
		t.Fatal("test Pro subscription not visible through REST")
	}
}

func (f *fixture) problem(t *testing.T, languages ...string) string {
	t.Helper()
	id, err := f.author.Problem(t.Context(), languages)
	must(t, err)
	return id
}

func finished(t *testing.T, c *driver.Client, problem, language, source string) *v1.Submission {
	t.Helper()
	sub, err := c.Submit(t.Context(), problem, language, source)
	must(t, err)
	result, err := c.Watch(t.Context(), sub.GetId())
	must(t, err)
	return result
}

func (f *fixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, f.database)
	must(t, err)
	_, execErr := conn.Exec(ctx, query, args...)
	closeErr := conn.Close(ctx)
	must(t, errors.Join(execErr, closeErr))
}
