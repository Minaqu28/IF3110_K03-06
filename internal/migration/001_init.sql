CREATE TABLE users (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username          TEXT        NOT NULL UNIQUE,
    password_hash     TEXT        NOT NULL,
    is_platform_admin BOOLEAN     NOT NULL DEFAULT FALSE,
    is_active         BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT users_username_trimmed CHECK (username = btrim(username) AND username <> '')
);
 
CREATE TABLE groups (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       TEXT        NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT groups_name_not_blank CHECK (btrim(name) <> '')
);
 
CREATE TABLE group_members (
    group_id BIGINT NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    user_id  BIGINT NOT NULL REFERENCES users (id)  ON DELETE CASCADE,
    PRIMARY KEY (group_id, user_id)
);
CREATE INDEX group_members_user_id_idx ON group_members (user_id);

CREATE TABLE database_connections (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name               TEXT        NOT NULL UNIQUE,
    host               TEXT        NOT NULL,
    port               INTEGER     NOT NULL DEFAULT 5432,
    db_name            TEXT        NOT NULL,
    db_user            TEXT        NOT NULL,
    -- nonce || ciphertext (AES-GCM). Jangan pernah ditampilkan atau di-log.
    password_encrypted BYTEA       NOT NULL,
    ssl_mode           TEXT        NOT NULL DEFAULT 'prefer',
    created_by         BIGINT      REFERENCES users (id) ON DELETE SET NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT database_connections_port_range CHECK (port BETWEEN 1 AND 65535),
    CONSTRAINT database_connections_ssl_mode CHECK (
        ssl_mode IN ('disable', 'allow', 'prefer', 'require', 'verify-ca', 'verify-full')
    )
);

CREATE TABLE group_database_access (
    group_id     BIGINT      NOT NULL REFERENCES groups (id)               ON DELETE CASCADE,
    database_id  BIGINT      NOT NULL REFERENCES database_connections (id) ON DELETE CASCADE,
    access_level TEXT        NOT NULL,
    granted_by   BIGINT      REFERENCES users (id) ON DELETE SET NULL,
    granted_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, database_id),
    CONSTRAINT group_database_access_level CHECK (access_level IN ('VIEW_ONLY', 'VIEW_EDIT'))
);
CREATE INDEX group_database_access_database_id_idx ON group_database_access (database_id);
 
CREATE TABLE sessions (
    id                  TEXT        PRIMARY KEY,
    user_id             BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    identifier_hash     BYTEA       NOT NULL UNIQUE,
    previous_hash       BYTEA       UNIQUE,            -- identifier sebelum rotation (grace period)
    csrf_token          TEXT        NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    rotated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    absolute_expires_at TIMESTAMPTZ NOT NULL,          -- tidak pernah diperpanjang oleh rotation
    revoked_at          TIMESTAMPTZ,
    CONSTRAINT sessions_identifier_hash_len CHECK (octet_length(identifier_hash) = 32),
    CONSTRAINT sessions_previous_hash_len   CHECK (previous_hash IS NULL OR octet_length(previous_hash) = 32),
    CONSTRAINT sessions_absolute_after_created CHECK (absolute_expires_at > created_at)
);
CREATE INDEX sessions_user_id_idx             ON sessions (user_id);
CREATE INDEX sessions_absolute_expires_at_idx ON sessions (absolute_expires_at);
 
CREATE TABLE migrations (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    database_id   BIGINT      NOT NULL REFERENCES database_connections (id) ON DELETE RESTRICT,
    version       INTEGER     NOT NULL,
    title         TEXT        NOT NULL DEFAULT '',
    sql           TEXT        NOT NULL,
    status        TEXT        NOT NULL DEFAULT 'DRAFT',
    error_message TEXT,
    created_by    BIGINT      REFERENCES users (id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    executed_by   BIGINT      REFERENCES users (id) ON DELETE SET NULL,
    executed_at   TIMESTAMPTZ,
    UNIQUE (database_id, version),
    CONSTRAINT migrations_version_positive CHECK (version > 0),
    CONSTRAINT migrations_sql_not_blank    CHECK (btrim(sql) <> ''),
    CONSTRAINT migrations_status           CHECK (status IN ('DRAFT', 'SUCCESS', 'FAILED')),
    CONSTRAINT migrations_executed_fields  CHECK (
        status = 'DRAFT' OR executed_at IS NOT NULL
    )
);
CREATE INDEX migrations_database_version_idx ON migrations (database_id, version DESC);

