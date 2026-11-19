package material

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestRepositoryLifecycle(t *testing.T) {
	// arrange
	f := setup(t)
	created := f.create(t, false, 3)
	// act
	loaded, err := f.repo.Get(t.Context(), created.ID)
	// assert
	if err != nil || loaded.Title != created.Title || len(loaded.ProblemIDs) != 2 || loaded.IsPublished || loaded.CreatedAt.IsZero() {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	replay := created
	replay.Title = "Повтор"
	replay.ProblemIDs = []uuid.UUID{uuid.New()}
	repeated, err := f.repo.Create(t.Context(), replay)
	if err != nil || repeated.ID != created.ID || repeated.Title != created.Title {
		t.Fatalf("replay=%+v err=%v", repeated, err)
	}
	loaded.Title = "Обновлено"
	loaded.ProblemIDs = []uuid.UUID{f.secondID}
	loaded.IsPublished = true
	updated, err := f.repo.Update(t.Context(), loaded)
	if err != nil || !updated.IsPublished || updated.Title != loaded.Title || len(updated.ProblemIDs) != 1 || updated.ProblemIDs[0] != f.secondID || !updated.CreatedAt.Equal(created.CreatedAt) || !updated.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	loaded.ProblemIDs = []uuid.UUID{uuid.New()}
	if _, err := f.repo.Update(t.Context(), loaded); !errors.Is(err, ErrUnknownProblem) {
		t.Fatalf("foreign key=%v", err)
	}
	still, err := f.repo.Get(t.Context(), created.ID)
	if err != nil || len(still.ProblemIDs) != 1 || still.ProblemIDs[0] != f.secondID {
		t.Fatalf("rollback=%+v err=%v", still, err)
	}
	if err := f.repo.Delete(t.Context(), created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Get(t.Context(), created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted=%v", err)
	}
}

func TestRepositoryPagination(t *testing.T) {
	f := setup(t)
	first := f.create(t, true, 0)
	second := f.create(t, true, 0)
	f.create(t, false, 0)
	last := f.create(t, true, 5)
	page, err := f.repo.List(t.Context(), ListFilter{ProblemID: f.problemID, Limit: 2})
	if err != nil || len(page) != 2 || page[0].ID != first.ID || page[1].ID != second.ID || page[0].ContentMarkdown != "" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	page, err = f.repo.List(t.Context(), ListFilter{ProblemID: f.problemID, After: second.ID, Ordinal: second.Ordinal, Limit: 2})
	if err != nil || len(page) != 1 || page[0].ID != last.ID {
		t.Fatalf("next=%+v err=%v", page, err)
	}
	all, err := f.repo.List(t.Context(), ListFilter{All: true, Limit: 10})
	if err != nil || len(all) != 4 {
		t.Fatalf("all=%+v err=%v", all, err)
	}
	empty, err := f.repo.List(t.Context(), ListFilter{ProblemID: uuid.New(), Limit: 10})
	if err != nil || len(empty) != 0 {
		t.Fatalf("filter=%+v err=%v", empty, err)
	}
}

func TestRepositoryAttachmentIsolation(t *testing.T) {
	f := setup(t)
	material := f.create(t, true, 0)
	other := f.create(t, true, 1)
	attachment, err := f.repo.CreateAttachment(t.Context(), Attachment{MaterialID: material.ID, RequestID: uuid.New(), Filename: "test.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.Attachment(t.Context(), other.ID, attachment.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross material=%v", err)
	}
	attachment.ObjectKey = "confirmed/first"
	attachment.ContentType = "application/pdf"
	attachment.SizeBytes = 10
	confirmed, err := f.repo.ConfirmAttachment(t.Context(), attachment)
	if err != nil || !confirmed.IsReady {
		t.Fatalf("confirmed=%+v err=%v", confirmed, err)
	}
	attachment.ObjectKey = "confirmed/second"
	repeated, err := f.repo.ConfirmAttachment(t.Context(), attachment)
	if err != nil || repeated.ObjectKey != confirmed.ObjectKey {
		t.Fatalf("overwrite=%+v err=%v", repeated, err)
	}
	if err := f.repo.Delete(t.Context(), material.ID); err != nil {
		t.Fatal(err)
	}
	attachments, err := f.repo.Attachments(t.Context(), material.ID)
	if err != nil || len(attachments) != 0 {
		t.Fatalf("cascade=%+v err=%v", attachments, err)
	}
}

func TestMigrationEntitlement(t *testing.T) {
	f := setup(t)
	var free, pro bool
	if err := f.pool.QueryRow(t.Context(), `SELECT has_material_access FROM plans WHERE code='free'`).Scan(&free); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT has_material_access FROM plans WHERE code='pro'`).Scan(&pro); err != nil {
		t.Fatal(err)
	}
	if free || !pro {
		t.Fatalf("free=%v pro=%v", free, pro)
	}
	var tables int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name IN ('materials','material_problems','material_attachments')`).Scan(&tables); err != nil || tables != 3 {
		t.Fatalf("schema tables=%d err=%v", tables, err)
	}
}
