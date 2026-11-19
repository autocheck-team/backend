-- +goose Up
CREATE TABLE problem_embeddings (
    problem_id uuid PRIMARY KEY REFERENCES problems (id) ON DELETE CASCADE,
    content_hash text NOT NULL,
    model text NOT NULL,
    embedding real[],
    lease_token uuid NOT NULL,
    lease_until timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT problem_embeddings_check_hash CHECK (length(content_hash) = 64),
    CONSTRAINT problem_embeddings_check_vector CHECK (
        embedding IS NULL OR (cardinality(embedding) > 0 AND array_ndims(embedding) = 1
            AND array_position(embedding, NULL) IS NULL)
    )
);

-- Храним только счётчики, без учебных целей и ответов провайдера.
CREATE TABLE recommendation_usage (
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    usage_on date NOT NULL,
    requests integer NOT NULL CONSTRAINT recommendation_usage_check_requests CHECK (requests > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT recommendation_usage_pkey PRIMARY KEY (user_id, usage_on)
);
CREATE INDEX recommendation_usage_idx_usage_on ON recommendation_usage (usage_on);

-- +goose Down
-- Обратные миграции запрещены.
