CREATE TABLE migration_attempts (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    migration_id  BIGINT      NOT NULL REFERENCES migrations (id) ON DELETE RESTRICT,
    database_id   BIGINT      NOT NULL REFERENCES database_connections (id) ON DELETE RESTRICT,
    user_id       BIGINT      REFERENCES users (id) ON DELETE SET NULL,
    sql_snapshot  TEXT        NOT NULL,
    status        TEXT        NOT NULL DEFAULT 'RUNNING',
    error_message TEXT,
    started_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at   TIMESTAMPTZ,
    CONSTRAINT migration_attempts_status CHECK (status IN ('RUNNING', 'SUCCESS', 'FAILED', 'UNKNOWN')),
    CONSTRAINT migration_attempts_finished CHECK (
        (status = 'RUNNING' AND finished_at IS NULL)
        OR (status <> 'RUNNING' AND finished_at IS NOT NULL)
    )
);
CREATE UNIQUE INDEX migration_attempts_one_running_idx ON migration_attempts (migration_id) WHERE status = 'RUNNING';
CREATE INDEX migration_attempts_database_started_idx ON migration_attempts (database_id, started_at DESC);
