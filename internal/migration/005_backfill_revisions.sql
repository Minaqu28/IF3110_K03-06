CREATE FUNCTION pg_temp.hash_field(value TEXT) RETURNS BYTEA
LANGUAGE sql IMMUTABLE AS $$
    SELECT int8send(octet_length(convert_to(value, 'UTF8'))::bigint) || convert_to(value, 'UTF8')
$$;

INSERT INTO migration_revisions (
    migration_id, database_id, revision, title, up_sql, down_sql, reversible, irreversible_reason,
    status, definition_hash, created_by, created_at, locked_at
)
SELECT
    m.id,
    m.database_id,
    1,
    m.title,
    m.sql,
    NULL,
    FALSE,
    'Migration dibuat sebelum revision dan down SQL tersedia.',
    CASE WHEN m.status = 'SUCCESS' THEN 'APPROVED' ELSE 'DRAFT' END,
    encode(sha256(
        pg_temp.hash_field('dbmp-revision-v1')
        || pg_temp.hash_field(m.sql)
        || pg_temp.hash_field('')
        || pg_temp.hash_field('false')
        || pg_temp.hash_field('NORMAL')
    ), 'hex'),
    m.created_by,
    m.created_at,
    CASE WHEN m.status = 'SUCCESS' THEN COALESCE(m.executed_at, m.updated_at) END
FROM migrations m
WHERE NOT EXISTS (SELECT 1 FROM migration_revisions r WHERE r.migration_id = m.id);

DROP FUNCTION pg_temp.hash_field(TEXT);
