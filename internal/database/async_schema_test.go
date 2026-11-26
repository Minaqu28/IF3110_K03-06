package database_test

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migration"
	"github.com/jackc/pgx/v5/pgconn"
)

type asyncFixture struct {
	userID      int64
	databaseID  int64
	migrationID int64
}

func newAsyncFixture(t *testing.T, db *sql.DB) asyncFixture {
	t.Helper()
	ctx := context.Background()
	f := asyncFixture{userID: insertUser(t, db, "author", "author-long-password", false).ID}
	if err := db.QueryRowContext(ctx, `
		INSERT INTO database_connections (name, host, db_name, db_user, password_encrypted)
		VALUES ('target', 'localhost', 'target', 'target', $1) RETURNING id`, []byte{0}).Scan(&f.databaseID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `
		INSERT INTO migrations (database_id, version, sql, created_by)
		VALUES ($1, 1, 'SELECT 1', $2) RETURNING id`, f.databaseID, f.userID).Scan(&f.migrationID); err != nil {
		t.Fatal(err)
	}
	return f
}

var selectOneHash = domain.ComputeDefinitionHash(domain.MigrationRevision{UpSQL: "SELECT 1"})

func (f asyncFixture) insertRevision(t *testing.T, db *sql.DB, revision int, locked bool) int64 {
	t.Helper()
	status, lockedAt := "DRAFT", "NULL"
	if locked {
		status, lockedAt = "APPROVED", "now()"
	}
	var id int64
	err := db.QueryRowContext(context.Background(), `
		INSERT INTO migration_revisions
		    (migration_id, database_id, revision, up_sql, irreversible_reason, status, definition_hash, created_by, locked_at)
		VALUES ($1, $2, $3, 'SELECT 1', 'test', $4, $5, $6, `+lockedAt+`) RETURNING id`,
		f.migrationID, f.databaseID, revision, status, selectOneHash, f.userID).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f asyncFixture) insertReview(t *testing.T, db *sql.DB, revisionID int64) int64 {
	t.Helper()
	var id int64
	err := db.QueryRowContext(context.Background(), `
		INSERT INTO review_requests (revision_id, definition_hash, status, requested_by, resolved_at)
		VALUES ($1, $2, 'APPROVED', $3, now()) RETURNING id`, revisionID, selectOneHash, f.userID).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f asyncFixture) insertExecution(db *sql.DB, revisionID, reviewID int64, action, status string, target *string) (string, error) {
	var id string
	err := db.QueryRowContext(context.Background(), `
		INSERT INTO migration_executions
		    (database_id, migration_id, revision_id, approval_request_id, action, mode, status, target_execution_id, requested_by)
		VALUES ($1, $2, $3, $4, $5, 'NORMAL', $6, $7, $8) RETURNING id`,
		f.databaseID, f.migrationID, revisionID, reviewID, action, status, target, f.userID).Scan(&id)
	return id, err
}

func requireSQLState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("err = %v, want SQLSTATE %s", err, code)
	}
}

func TestRevisionConstraintsPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	ctx := context.Background()
	cases := map[string]struct{ columns, values string }{
		"half batch config":   {"batch_table", "'public.orders'"},
		"batch size too big":  {"batch_table, batch_cursor_column, batch_size", "'public.orders', 'id', 100001"},
		"reversible no down":  {"reversible", "TRUE"},
		"blank down sql":      {"reversible, down_sql", "TRUE, '  '"},
		"approved not locked": {"locked_at", "NULL"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			status := "'DRAFT'"
			if name == "approved not locked" {
				status = "'APPROVED'"
			}
			_, err := db.ExecContext(ctx, `INSERT INTO migration_revisions
				(migration_id, database_id, revision, up_sql, irreversible_reason, definition_hash, status, `+c.columns+`)
				VALUES ($1, $2, 1, 'SELECT 1', 'x', $3, `+status+`, `+c.values+`)`,
				f.migrationID, f.databaseID, selectOneHash)
			requireSQLState(t, err, "23514")
		})
	}
	_, err := db.ExecContext(ctx, `INSERT INTO migration_revisions
		(migration_id, database_id, revision, up_sql, irreversible_reason, definition_hash)
		VALUES ($1, $2, 1, 'SELECT 1', 'x', 'not-hex')`, f.migrationID, f.databaseID)
	requireSQLState(t, err, "23514")
}

func TestRevisionImmutabilityPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	ctx := context.Background()
	draft := f.insertRevision(t, db, 1, false)
	locked := f.insertRevision(t, db, 2, true)

	if _, err := db.ExecContext(ctx, `UPDATE migration_revisions SET up_sql = 'SELECT 2' WHERE id = $1`, draft); err != nil {
		t.Fatalf("draft revision should stay editable: %v", err)
	}
	for name, stmt := range map[string]string{
		"up_sql":    `UPDATE migration_revisions SET up_sql = 'SELECT 2' WHERE id = $1`,
		"hash":      `UPDATE migration_revisions SET definition_hash = repeat('0', 64) WHERE id = $1`,
		"batch":     `UPDATE migration_revisions SET batch_table = 'public.t', batch_cursor_column = 'id', batch_size = 10 WHERE id = $1`,
		"unlock":    `UPDATE migration_revisions SET locked_at = NULL, status = 'DRAFT' WHERE id = $1`,
		"delete":    `DELETE FROM migration_revisions WHERE id = $1`,
		"retarget":  `UPDATE migration_revisions SET revision = 9 WHERE id = $1`,
		"reversing": `UPDATE migration_revisions SET reversible = TRUE, down_sql = 'SELECT 0' WHERE id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, stmt, locked)
			requireSQLState(t, err, "23514")
		})
	}
	if _, err := db.ExecContext(ctx, `UPDATE migration_revisions SET status = 'REJECTED', title = 'renamed' WHERE id = $1`, locked); err != nil {
		t.Fatalf("status and title of a locked revision should be updatable: %v", err)
	}
}

func TestBackfillRevisionsPostgres(t *testing.T) {
	db := isolatedDB(t, false)
	ctx := context.Background()
	before := fstest.MapFS{}
	for _, name := range []string{"001_init.sql", "002_group_leaders.sql", "003_migration_revisions.sql", "004_revision_immutable.sql"} {
		body, err := fs.ReadFile(migration.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		before[name] = &fstest.MapFile{Data: body}
	}
	if err := database.Migrate(ctx, db, before); err != nil {
		t.Fatal(err)
	}
	f := newAsyncFixture(t, db)
	const appliedSQL = "CREATE TABLE ünïcode (id int)"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO migrations (database_id, version, title, sql, status, created_by, executed_by, executed_at)
		VALUES ($1, 2, 'applied', $2, 'SUCCESS', $3, $3, now())`, f.databaseID, appliedSQL, f.userID); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, db, migration.Files); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, db, migration.Files); err != nil {
		t.Fatalf("rerun: %v", err)
	}

	rows, err := db.QueryContext(ctx, `
		SELECT m.version, r.revision, r.up_sql, r.status, r.locked_at IS NOT NULL, r.reversible, r.definition_hash
		FROM migration_revisions r JOIN migrations m ON m.id = r.migration_id ORDER BY m.version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct {
		version, revision int
		upSQL, status     string
		locked            bool
		reversible        bool
		hash              string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.version, &r.revision, &r.upSQL, &r.status, &r.locked, &r.reversible, &r.hash); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("backfilled %d revisions, want 2: %+v", len(got), got)
	}
	want := []row{
		{1, 1, "SELECT 1", "DRAFT", false, false, selectOneHash},
		{2, 1, appliedSQL, "APPROVED", true, false, domain.ComputeDefinitionHash(domain.MigrationRevision{UpSQL: appliedSQL})},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("revision %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestExecutionConstraintsPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	revision := f.insertRevision(t, db, 1, true)
	review := f.insertReview(t, db, revision)

	applied, err := f.insertExecution(db, revision, review, "APPLY", "SUCCEEDED", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.insertExecution(db, revision, review, "ROLLBACK", "PENDING", nil); err == nil {
		t.Fatal("rollback without target execution should be rejected")
	} else {
		requireSQLState(t, err, "23514")
	}
	_, err = f.insertExecution(db, revision, review, "APPLY", "PENDING", &applied)
	requireSQLState(t, err, "23514")
	_, err = f.insertExecution(db, revision, review, "APPLY", "PARTIALLY_REVERTED", nil)
	requireSQLState(t, err, "23514")
	if _, err := f.insertExecution(db, revision, review, "ROLLBACK", "PARTIALLY_REVERTED", &applied); err != nil {
		t.Fatalf("partially reverted rollback should be accepted: %v", err)
	}
}

func TestExecutionAttemptsAppendOnlyPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	ctx := context.Background()
	revision := f.insertRevision(t, db, 1, true)
	execution, err := f.insertExecution(db, revision, f.insertReview(t, db, revision), "APPLY", "RUNNING", nil)
	if err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO execution_attempts (execution_id, database_id, attempt_number, status, sql_snapshot, started_at)
		VALUES ($1, $2, $3, 'FAILED', 'SELECT 1', now())`
	if _, err := db.ExecContext(ctx, insert, execution, f.databaseID, 1); err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, insert, execution, f.databaseID, 1)
	requireSQLState(t, err, "23505")
	for name, stmt := range map[string]string{
		"update":   `UPDATE execution_attempts SET status = 'SUCCEEDED'`,
		"delete":   `DELETE FROM execution_attempts`,
		"truncate": `TRUNCATE execution_attempts`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, stmt)
			requireSQLState(t, err, "23514")
		})
	}
}

func TestJobsAndLocksPostgres(t *testing.T) {
	db := integrationDB(t)
	f := newAsyncFixture(t, db)
	ctx := context.Background()
	revision := f.insertRevision(t, db, 1, true)
	execution, err := f.insertExecution(db, revision, f.insertReview(t, db, revision), "APPLY", "PENDING", nil)
	if err != nil {
		t.Fatal(err)
	}

	var jobID, status string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO migration_jobs (execution_id, database_id, idempotency_key, progress)
		VALUES ($1, $2, 'key-1', '{"status":"PENDING","completed":0,"affected_rows":0}') RETURNING id, status`,
		execution, f.databaseID).Scan(&jobID, &status); err != nil {
		t.Fatal(err)
	}
	if status != string(domain.JobStatusQueued) || len(jobID) != 36 {
		t.Fatalf("job = %s %s, want a uuid in QUEUED", jobID, status)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO migration_jobs (execution_id, database_id, idempotency_key) VALUES ($1, $2, 'key-1')`, execution, f.databaseID)
	requireSQLState(t, err, "23505")
	_, err = db.ExecContext(ctx, `UPDATE migration_jobs SET status = 'DONE' WHERE id = $1`, jobID)
	requireSQLState(t, err, "23514")
	_, err = db.ExecContext(ctx, `UPDATE migration_jobs SET progress = '[]' WHERE id = $1`, jobID)
	requireSQLState(t, err, "23514")

	lock := `INSERT INTO database_locks (database_id, execution_id, owner, fencing_token, lease_expires_at)
		VALUES ($1, $2, $3, nextval('database_lock_fencing_seq'), now() + interval '30 seconds')`
	if _, err := db.ExecContext(ctx, lock, f.databaseID, execution, "worker-a"); err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, lock, f.databaseID, execution, "worker-b")
	requireSQLState(t, err, "23505")
}
