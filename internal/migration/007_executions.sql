CREATE TABLE migration_executions (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    database_id         BIGINT      NOT NULL REFERENCES database_connections (id) ON DELETE RESTRICT,
    migration_id        BIGINT      NOT NULL REFERENCES migrations (id) ON DELETE RESTRICT,
    revision_id         BIGINT      NOT NULL REFERENCES migration_revisions (id) ON DELETE RESTRICT,
    approval_request_id BIGINT      NOT NULL REFERENCES review_requests (id) ON DELETE RESTRICT,
    action              TEXT        NOT NULL,
    mode                TEXT        NOT NULL,
    target_execution_id UUID        REFERENCES migration_executions (id) ON DELETE RESTRICT,
    status              TEXT        NOT NULL DEFAULT 'PENDING',
    scope_min           BIGINT,
    scope_max           BIGINT,
    applied_batches     INTEGER     NOT NULL DEFAULT 0,
    affected_rows       BIGINT      NOT NULL DEFAULT 0,
    last_cursor         TEXT,
    error_code          TEXT,
    error_message       TEXT,
    requested_by        BIGINT      REFERENCES users (id) ON DELETE SET NULL,
    requested_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at          TIMESTAMPTZ,
    finished_at         TIMESTAMPTZ,
    CONSTRAINT migration_executions_action CHECK (action IN ('APPLY', 'ROLLBACK', 'FORWARD')),
    CONSTRAINT migration_executions_mode   CHECK (mode IN ('NORMAL', 'BATCH')),
    CONSTRAINT migration_executions_status CHECK (
        status IN ('PENDING', 'RUNNING', 'SUCCEEDED', 'FAILED', 'PARTIAL', 'UNKNOWN', 'PARTIALLY_REVERTED')
    ),
    CONSTRAINT migration_executions_target  CHECK ((action = 'ROLLBACK') = (target_execution_id IS NOT NULL)),
    CONSTRAINT migration_executions_not_self CHECK (target_execution_id IS DISTINCT FROM id),
    CONSTRAINT migration_executions_scope   CHECK (
        (scope_min IS NULL AND scope_max IS NULL)
        OR (scope_min IS NOT NULL AND scope_max IS NOT NULL AND scope_min <= scope_max)
    ),
    CONSTRAINT migration_executions_counts  CHECK (applied_batches >= 0 AND affected_rows >= 0),
    CONSTRAINT migration_executions_partially_reverted CHECK (status <> 'PARTIALLY_REVERTED' OR action = 'ROLLBACK')
);
CREATE INDEX migration_executions_database_idx ON migration_executions (database_id, requested_at DESC);
CREATE INDEX migration_executions_open_idx ON migration_executions (database_id)
    WHERE status IN ('PENDING', 'RUNNING', 'PARTIAL', 'UNKNOWN', 'PARTIALLY_REVERTED');
