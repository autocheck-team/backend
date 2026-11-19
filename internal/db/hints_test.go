package db

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestHintSchema(t *testing.T) {
	// arrange
	_, tx := setup(t)
	submissionID := seed(t, tx)
	var allowed bool
	if err := tx.QueryRow(t.Context(), `SELECT p.allow_ai_hints FROM submissions s JOIN problems p ON p.id=s.problem_id WHERE s.id=$1`, submissionID).Scan(&allowed); err != nil || allowed {
		t.Fatal("hints enabled by default")
	}
	for code, want := range map[string]int{"free": 0, "pro": 20} {
		var limit int
		if err := tx.QueryRow(t.Context(), `SELECT daily_ai_hint_limit FROM plans WHERE code=$1`, code).Scan(&limit); err != nil || limit != want {
			t.Fatalf("plan %s limit=%d err=%v", code, limit, err)
		}
	}
	// act
	var id int64
	err := tx.QueryRow(t.Context(), `INSERT INTO submission_hints (submission_id,user_id,lease_token,lease_until)
      SELECT id,user_id,$2,now()+interval '1 minute' FROM submissions WHERE id=$1 RETURNING id`, submissionID, uuid.New()).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, query string
		args        []any
		code        string
	}{
		{"duplicate_submission", `INSERT INTO submission_hints (submission_id,user_id,lease_token,lease_until)
          SELECT id,user_id,$2,now()+interval '1 minute' FROM submissions WHERE id=$1`, []any{submissionID, uuid.New()}, "23505"},
		{"negative_limit", `UPDATE plans SET daily_ai_hint_limit=-1 WHERE code='free'`, nil, "23514"},
		{"oversized_text", `UPDATE submission_hints SET text=$2,lease_token=NULL,lease_until=NULL WHERE id=$1`, []any{id, strings.Repeat("я", 1001)}, "23514"},
		{"invalid_state", `UPDATE submission_hints SET lease_token=NULL WHERE id=$1`, []any{id}, "23514"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			savepoint, err := tx.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			_, err = savepoint.Exec(t.Context(), tc.query, tc.args...)
			if pgCode(err) != tc.code {
				t.Fatalf("code=%s err=%v", pgCode(err), err)
			}
			if err := savepoint.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
	if _, err := tx.Exec(t.Context(), `UPDATE submission_hints SET text='Проверьте границы.',lease_token=NULL,lease_until=NULL WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	// assert
	var text string
	var leaseEmpty bool
	if err := tx.QueryRow(t.Context(), `SELECT text,lease_token IS NULL AND lease_until IS NULL FROM submission_hints WHERE id=$1`, id).Scan(&text, &leaseEmpty); err != nil || text == "" || !leaseEmpty {
		t.Fatal("completed state invalid")
	}
	t.Log("00014: false by default, free=0/pro=20, identity, uniqueness, length and reservation constraints verified on PostgreSQL")
}
