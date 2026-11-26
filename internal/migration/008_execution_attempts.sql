CREATE TABLE execution_attempts (
    attempt_id     BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    execution_id   UUID        NOT NULL REFERENCES migration_executions (id) ON DELETE RESTRICT,
    database_id    BIGINT      NOT NULL REFERENCES database_connections (id) ON DELETE RESTRICT,
    attempt_number INTEGER     NOT NULL,
    status         TEXT        NOT NULL,
    sql_snapshot   TEXT        NOT NULL,
    error_code     TEXT,
    error_message  TEXT,
    started_at     TIMESTAMPTZ NOT NULL,
    finished_at    TIMESTAMPTZ,
    UNIQUE (execution_id, attempt_number),
    CONSTRAINT execution_attempts_number_positive CHECK (attempt_number > 0),
    CONSTRAINT execution_attempts_status CHECK (
        status IN ('PENDING', 'RUNNING', 'SUCCEEDED', 'FAILED', 'PARTIAL', 'UNKNOWN', 'PARTIALLY_REVERTED')
    ),
    CONSTRAINT execution_attempts_finished_after_start CHECK (finished_at IS NULL OR finished_at >= started_at)
);
CREATE INDEX execution_attempts_database_idx ON execution_attempts (database_id, started_at DESC);

CREATE FUNCTION forbid_execution_attempt_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'execution_attempts is append-only' USING ERRCODE = 'check_violation';
END
$$;

CREATE TRIGGER execution_attempts_append_only
    BEFORE UPDATE OR DELETE ON execution_attempts
    FOR EACH ROW EXECUTE FUNCTION forbid_execution_attempt_change();

CREATE TRIGGER execution_attempts_no_truncate
    BEFORE TRUNCATE ON execution_attempts
    FOR EACH STATEMENT EXECUTE FUNCTION forbid_execution_attempt_change();
