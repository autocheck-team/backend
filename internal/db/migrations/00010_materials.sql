-- +goose Up
ALTER TABLE plans ADD COLUMN has_material_access boolean NOT NULL DEFAULT false;
UPDATE plans SET has_material_access = true, updated_at = now() WHERE code = 'pro';

CREATE TABLE materials (
    id uuid CONSTRAINT materials_pkey PRIMARY KEY DEFAULT uuidv7(),
    request_id uuid NOT NULL CONSTRAINT materials_ukey_request_id UNIQUE,
    title text NOT NULL CONSTRAINT materials_check_title CHECK (char_length(btrim(title)) BETWEEN 1 AND 200),
    description text NOT NULL DEFAULT '' CONSTRAINT materials_check_description CHECK (char_length(description) <= 2000),
    content_markdown text NOT NULL CONSTRAINT materials_check_content CHECK (char_length(btrim(content_markdown)) BETWEEN 1 AND 200000),
    ordinal integer NOT NULL DEFAULT 0 CONSTRAINT materials_check_ordinal CHECK (ordinal >= 0),
    is_published boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX materials_idx_ordinal_id ON materials (ordinal, id);
CREATE INDEX materials_idx_published_ordinal_id ON materials (ordinal, id) WHERE is_published;

CREATE TABLE material_problems (
    material_id uuid NOT NULL CONSTRAINT material_problems_fkey_material_id REFERENCES materials (id) ON DELETE CASCADE,
    problem_id uuid NOT NULL CONSTRAINT material_problems_fkey_problem_id REFERENCES problems (id),
    CONSTRAINT material_problems_pkey PRIMARY KEY (material_id, problem_id)
);
CREATE INDEX material_problems_idx_problem_id_material_id ON material_problems (problem_id, material_id);

CREATE TABLE material_attachments (
    id uuid CONSTRAINT material_attachments_pkey PRIMARY KEY DEFAULT uuidv7(),
    material_id uuid NOT NULL CONSTRAINT material_attachments_fkey_material_id REFERENCES materials (id) ON DELETE CASCADE,
    request_id uuid NOT NULL,
    filename text NOT NULL CONSTRAINT material_attachments_check_filename CHECK (char_length(filename) BETWEEN 1 AND 255),
    object_key text NOT NULL DEFAULT '',
    content_type text NOT NULL DEFAULT '',
    size_bytes bigint NOT NULL DEFAULT 0 CONSTRAINT material_attachments_check_size CHECK (size_bytes >= 0),
    is_ready boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT material_attachments_ukey_material_id_request_id UNIQUE (material_id, request_id),
    CONSTRAINT material_attachments_check_ready CHECK (NOT is_ready OR (size_bytes > 0 AND object_key <> '' AND content_type <> ''))
);
CREATE INDEX material_attachments_idx_material_id_id ON material_attachments (material_id, id);

-- +goose Down
-- Обратные миграции запрещены.
