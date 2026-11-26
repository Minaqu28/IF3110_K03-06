CREATE TABLE migration_jobs (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    execution_id    UUID        NOT NULL REFERENCES migration_executions (id) ON DELETE RESTRICT,
    database_id     BIGINT      NOT NULL REFERENCES database_connections (id) ON DELETE RESTRICT,
    status          TEXT        NOT NULL DEFAULT 'QUEUED',
    attempt_count   INTEGER     NOT NULL DEFAULT 0,
    max_attempts    INTEGER     NOT NULL DEFAULT 5,
    progress        JSONB,
    error_code      TEXT,
    error_message   TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at    TIMESTAMPTZ,
    idempotency_key TEXT        UNIQUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT migration_jobs_status CHECK (
        status IN ('QUEUED', 'RUNNING', 'RETRY_WAIT', 'SUCCEEDED', 'FAILED', 'NEEDS_RECONCILIATION', 'CANCELLED')
    ),
    CONSTRAINT migration_jobs_attempts CHECK (attempt_count >= 0 AND max_attempts > 0),
    CONSTRAINT migration_jobs_progress_object CHECK (progress IS NULL OR jsonb_typeof(progress) = 'object')
);
CREATE INDEX migration_jobs_execution_idx ON migration_jobs (execution_id, created_at DESC);
CREATE INDEX migration_jobs_outbox_idx ON migration_jobs (next_attempt_at)
    WHERE published_at IS NULL AND status IN ('QUEUED', 'RETRY_WAIT', 'NEEDS_RECONCILIATION');
CREATE INDEX migration_jobs_running_idx ON migration_jobs (database_id) WHERE status = 'RUNNING';
