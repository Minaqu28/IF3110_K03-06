CREATE SEQUENCE database_lock_fencing_seq;

CREATE TABLE database_locks (
    database_id      BIGINT      PRIMARY KEY REFERENCES database_connections (id) ON DELETE CASCADE,
    execution_id     UUID        NOT NULL REFERENCES migration_executions (id) ON DELETE RESTRICT,
    owner            TEXT        NOT NULL,
    fencing_token    BIGINT      NOT NULL,
    lease_expires_at TIMESTAMPTZ NOT NULL,
    acquired_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT database_locks_owner_not_blank CHECK (btrim(owner) <> ''),
    CONSTRAINT database_locks_lease_after_acquire CHECK (lease_expires_at > acquired_at)
);
