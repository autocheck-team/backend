package material

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const attachmentColumns = `id,material_id,request_id,filename,object_key,content_type,size_bytes,is_ready`

func scanAttachment(row pgx.Row) (Attachment, error) {
	var attachment Attachment
	err := row.Scan(&attachment.ID, &attachment.MaterialID, &attachment.RequestID, &attachment.Filename,
		&attachment.ObjectKey, &attachment.ContentType, &attachment.SizeBytes, &attachment.IsReady)
	if err != nil {
		return Attachment{}, repositoryError(err)
	}
	return attachment, nil
}

// CreateAttachment резервирует вложение; повтор операции не меняет имя и готовность.
func (r *Repository) CreateAttachment(ctx context.Context, attachment Attachment) (Attachment, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return scanAttachment(r.pool.QueryRow(ctx, `INSERT INTO material_attachments (material_id,request_id,filename)
  VALUES ($1,$2,$3) ON CONFLICT (material_id,request_id) DO UPDATE SET request_id=excluded.request_id
  RETURNING `+attachmentColumns, attachment.MaterialID, attachment.RequestID, attachment.Filename))
}

// Attachment возвращает вложение только в пределах указанного материала.
func (r *Repository) Attachment(ctx context.Context, materialID, id uuid.UUID) (Attachment, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return scanAttachment(r.pool.QueryRow(ctx, `SELECT `+attachmentColumns+` FROM material_attachments
  WHERE material_id=$1 AND id=$2`, materialID, id))
}

// Attachments возвращает вложения в стабильном порядке.
func (r *Repository) Attachments(ctx context.Context, id uuid.UUID) ([]Attachment, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	rows, err := r.pool.Query(ctx, `SELECT `+attachmentColumns+` FROM material_attachments WHERE material_id=$1 ORDER BY id`, id)
	if err != nil {
		return nil, fmt.Errorf("list material attachments: %w", err)
	}
	attachments, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Attachment, error) { return scanAttachment(row) })
	if err != nil {
		return nil, fmt.Errorf("collect material attachments: %w", err)
	}
	return attachments, nil
}

// ConfirmAttachment фиксирует первую подтверждённую копию даже при параллельных confirm.
func (r *Repository) ConfirmAttachment(ctx context.Context, attachment Attachment) (Attachment, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return scanAttachment(r.pool.QueryRow(ctx, `UPDATE material_attachments
  SET object_key=CASE WHEN is_ready THEN object_key ELSE $3 END,
   content_type=CASE WHEN is_ready THEN content_type ELSE $4 END,
   size_bytes=CASE WHEN is_ready THEN size_bytes ELSE $5 END,is_ready=true,updated_at=clock_timestamp()
  WHERE material_id=$1 AND id=$2 RETURNING `+attachmentColumns,
		attachment.MaterialID, attachment.ID, attachment.ObjectKey, attachment.ContentType, attachment.SizeBytes))
}

// DeleteAttachment удаляет метаданные вложения.
func (r *Repository) DeleteAttachment(ctx context.Context, materialID, id uuid.UUID) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	tag, err := r.pool.Exec(ctx, `DELETE FROM material_attachments WHERE material_id=$1 AND id=$2`, materialID, id)
	if err != nil {
		return fmt.Errorf("delete material attachment: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
