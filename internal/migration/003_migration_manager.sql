-- Migration manager: execution time on migrations and a log of every change
-- executed against a managed database (migrations and SQL editor mutations).
ALTER TABLE migrations ADD COLUMN duration_ms BIGINT;
ALTER TABLE migrations ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE TABLE database_change_log (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    database_id       BIGINT      NOT NULL REFERENCES database_connections (id) ON DELETE CASCADE,
    migration_id      BIGINT      REFERENCES migrations (id) ON DELETE SET NULL,
    migration_version INTEGER,
    user_id           BIGINT      REFERENCES users (id) ON DELETE SET NULL,
    source            TEXT        NOT NULL,
    sql               TEXT        NOT NULL,
    status            TEXT        NOT NULL,
    error_message     TEXT,
    duration_ms       BIGINT      NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT database_change_log_source CHECK (source IN ('MIGRATION', 'SQL_EDITOR')),
    CONSTRAINT database_change_log_status CHECK (status IN ('SUCCESS', 'FAILED'))
);
CREATE INDEX database_change_log_database_idx ON database_change_log (database_id, created_at DESC);

-- Earlier versions recorded SQL editor mutations directly as migrations.
INSERT INTO database_change_log (database_id, migration_id, migration_version, user_id, source, sql, status, error_message, created_at)
SELECT database_id, id, version, executed_by, 'SQL_EDITOR', sql, status, error_message, executed_at
FROM migrations WHERE status <> 'DRAFT';
