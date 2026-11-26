CREATE TABLE migration_revisions (
    id                  BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    migration_id        BIGINT      NOT NULL REFERENCES migrations (id) ON DELETE RESTRICT,
    database_id         BIGINT      NOT NULL REFERENCES database_connections (id) ON DELETE RESTRICT,
    revision            INTEGER     NOT NULL,
    title               TEXT        NOT NULL DEFAULT '',
    up_sql              TEXT        NOT NULL,
    down_sql            TEXT,
    reversible          BOOLEAN     NOT NULL DEFAULT FALSE,
    irreversible_reason TEXT,
    status              TEXT        NOT NULL DEFAULT 'DRAFT',
    definition_hash     TEXT        NOT NULL,
    batch_table         TEXT,
    batch_cursor_column TEXT,
    batch_size          INTEGER,
    created_by          BIGINT      REFERENCES users (id) ON DELETE SET NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_at           TIMESTAMPTZ,
    UNIQUE (migration_id, revision),
    CONSTRAINT migration_revisions_revision_positive CHECK (revision > 0),
    CONSTRAINT migration_revisions_up_not_blank      CHECK (btrim(up_sql) <> ''),
    CONSTRAINT migration_revisions_status            CHECK (status IN ('DRAFT', 'IN_REVIEW', 'APPROVED', 'REJECTED')),
    CONSTRAINT migration_revisions_hash_hex          CHECK (definition_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT migration_revisions_down_when_reversible CHECK (reversible = (down_sql IS NOT NULL AND btrim(down_sql) <> '')),
    CONSTRAINT migration_revisions_irreversible_reason  CHECK (reversible OR irreversible_reason IS NOT NULL),
    CONSTRAINT migration_revisions_batch_complete CHECK (
        (batch_table IS NULL AND batch_cursor_column IS NULL AND batch_size IS NULL)
        OR (batch_table IS NOT NULL AND batch_cursor_column IS NOT NULL AND batch_size BETWEEN 1 AND 100000)
    ),
    CONSTRAINT migration_revisions_locked_after_draft CHECK (status = 'DRAFT' OR locked_at IS NOT NULL)
);
CREATE INDEX migration_revisions_database_idx ON migration_revisions (database_id, migration_id, revision DESC);
