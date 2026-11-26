CREATE TABLE review_requests (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    revision_id     BIGINT      NOT NULL REFERENCES migration_revisions (id) ON DELETE RESTRICT,
    definition_hash TEXT        NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'PENDING',
    requested_by    BIGINT      REFERENCES users (id) ON DELETE SET NULL,
    requested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at     TIMESTAMPTZ,
    CONSTRAINT review_requests_status   CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED', 'STALE')),
    CONSTRAINT review_requests_hash_hex CHECK (definition_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT review_requests_resolved CHECK ((status = 'PENDING') = (resolved_at IS NULL))
);
CREATE UNIQUE INDEX review_requests_one_pending_idx ON review_requests (revision_id) WHERE status = 'PENDING';
CREATE INDEX review_requests_revision_idx ON review_requests (revision_id, requested_at DESC);

CREATE TABLE approval_decisions (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    review_id  BIGINT      NOT NULL REFERENCES review_requests (id) ON DELETE CASCADE,
    group_id   BIGINT      NOT NULL REFERENCES groups (id) ON DELETE RESTRICT,
    leader_id  BIGINT      NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    status     TEXT        NOT NULL DEFAULT 'PENDING',
    comment    TEXT,
    decided_at TIMESTAMPTZ,
    UNIQUE (review_id, group_id),
    CONSTRAINT approval_decisions_status  CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED', 'STALE')),
    CONSTRAINT approval_decisions_decided CHECK (
        (status <> 'PENDING' OR decided_at IS NULL)
        AND (status NOT IN ('APPROVED', 'REJECTED') OR decided_at IS NOT NULL)
    )
);
CREATE INDEX approval_decisions_leader_idx ON approval_decisions (leader_id) WHERE status = 'PENDING';
