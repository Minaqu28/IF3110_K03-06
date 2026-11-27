package database

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestDescribeConnectErrorClassifiesAuthAndMissingDB(t *testing.T) {
	authErr := describeConnectError(&pgconn.PgError{Code: "28P01", Message: "password authentication failed"})
	if msg := explorer.ConnectionMessage(authErr); !strings.Contains(msg, "autentikasi database gagal") {
		t.Fatalf("unexpected auth error message: %q", msg)
	}
	dbErr := describeConnectError(&pgconn.PgError{Code: "3D000", Message: "database does not exist"})
	if msg := explorer.ConnectionMessage(dbErr); !strings.Contains(msg, "database tidak ditemukan") {
		t.Fatalf("unexpected db error message: %q", msg)
	}
}

func TestTargetDSNEscapesCredentials(t *testing.T) {
	dsn := TargetDSN(domain.Connection{Host: "db", Port: 5432, DBname: "app", DBuser: "u@x", SSLMode: "disable"}, "p@ss/w:rd")
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if config.User != "u@x" || config.Password != "p@ss/w:rd" || config.Database != "app" || config.Host != "db" {
		t.Fatalf("unexpected config: %s %s %s %s", config.User, config.Password, config.Database, config.Host)
	}
}

func openTestTarget(t *testing.T, readOnly bool) *pgTarget {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return connectTestTarget(t, config, readOnly)
}

func openIsolatedTargets(t *testing.T, count int) []*pgTarget {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	root, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("dbmp_target_%d", time.Now().UnixNano())
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := root.Exec(ctx, `CREATE DATABASE `+ident); err != nil {
		root.Close(ctx)
		t.Fatalf("create isolated target database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec(context.Background(), `DROP DATABASE `+ident+` WITH (FORCE)`)
		_ = root.Close(context.Background())
	})
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.Database = name
	targets := make([]*pgTarget, count)
	for i := range targets {
		targets[i] = connectTestTarget(t, config.Copy(), false)
	}
	return targets
}

func connectTestTarget(t *testing.T, config *pgx.ConnConfig, readOnly bool) *pgTarget {
	if readOnly {
		config.RuntimeParams["default_transaction_read_only"] = "on"
	}
	conn, err := pgx.ConnectConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return &pgTarget{conn: conn, readOnly: readOnly}
}

func TestTargetIntegration(t *testing.T) {
	ctx := context.Background()
	edit := openTestTarget(t, false)
	results := edit.Run(ctx, "DROP TABLE IF EXISTS explorer_it; CREATE TABLE explorer_it (id int, name text); INSERT INTO explorer_it VALUES (1, 'a'), (2, NULL); SELECT * FROM explorer_it ORDER BY id", 1)
	if len(results) != 4 || hasError(results) {
		t.Fatalf("results = %+v", results)
	}
	sel := results[3]
	if len(sel.Rows) != 1 || !sel.Truncated || sel.Columns[1] != "name" {
		t.Fatalf("select = %+v", sel)
	}

	results = edit.Run(ctx, "INSERT INTO explorer_it VALUES (3, 'c'); SELECT 1/0", 10)
	if !hasError(results) {
		t.Fatalf("expected error, got %+v", results)
	}
	edit.Run(ctx, "BEGIN; INSERT INTO explorer_it VALUES (4, 'd')", 10)

	tables, err := edit.Tables(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, table := range tables {
		if table.Name == "explorer_it" {
			found = true
			if table.Rows != 2 {
				t.Fatalf("rows = %d, want 2", table.Rows)
			}
		}
	}
	if !found {
		t.Fatal("explorer_it not listed")
	}

	preview, err := edit.Preview(ctx, "explorer_it", 50)
	if err != nil || len(preview.Rows) != 2 || !preview.Rows[1][1].Null {
		t.Fatalf("preview = %+v, err = %v", preview, err)
	}
	if _, err := edit.Preview(ctx, `explorer_it"; DROP TABLE explorer_it; --`, 50); err == nil {
		t.Fatal("expected table-not-found for injected name")
	}
	schema := fmt.Sprintf("explorer_meta_%d", time.Now().UnixNano())
	qualified := pgx.Identifier{schema, "items"}.Sanitize()
	if results := edit.Run(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()+
		"; CREATE TABLE "+qualified+" (id integer PRIMARY KEY, name text NOT NULL DEFAULT 'x')"+
		"; CREATE INDEX items_name_idx ON "+qualified+" (name)"+
		"; INSERT INTO "+qualified+" (id, name) VALUES (3, 'c'), (1, 'a'), (2, 'b')", 10); hasError(results) {
		t.Fatalf("setup schema: %+v", results)
	}
	t.Cleanup(func() {
		edit.Run(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE", 10)
	})
	schemas, err := edit.Schemas(ctx)
	if err != nil || !slices.Contains(schemas, schema) {
		t.Fatalf("schemas = %v, err = %v", schemas, err)
	}
	metaTables, err := edit.TablesInSchema(ctx, schema)
	if err != nil || len(metaTables) != 1 || metaTables[0].Schema != schema || metaTables[0].Rows != 3 {
		t.Fatalf("schema tables = %+v, err = %v", metaTables, err)
	}
	details, err := edit.TableDetails(ctx, schema, "items")
	if err != nil || len(details.Columns) != 2 || details.Columns[0].Nullable || len(details.Constraints) == 0 || len(details.Indexes) < 2 {
		t.Fatalf("table details = %+v, err = %v", details, err)
	}
	first, err := edit.PreviewPage(ctx, schema, "items", 2, 0)
	if err != nil || len(first.Rows) != 2 || !first.Truncated || first.Rows[0][0].Value != "1" {
		t.Fatalf("first page = %+v, err = %v", first, err)
	}
	second, err := edit.PreviewPage(ctx, schema, "items", 2, 2)
	if err != nil || len(second.Rows) != 1 || second.Truncated || second.Rows[0][0].Value != "3" {
		t.Fatalf("second page = %+v, err = %v", second, err)
	}
	if _, err := edit.TableDetails(ctx, schema, "missing"); !errors.Is(err, explorer.ErrTableNotFound) {
		t.Fatalf("missing table detail: %v", err)
	}

	ro := openTestTarget(t, true)
	for _, sql := range []string{
		"DELETE FROM explorer_it",
		"WITH d AS (DELETE FROM explorer_it RETURNING *) SELECT * FROM d",
		"SELECT 1; DELETE FROM explorer_it",
		"COMMIT; DELETE FROM explorer_it",
	} {
		res := ro.Run(ctx, sql, 10)
		if len(res) != 1 || res[0].Error == "" {
			t.Errorf("read-only Run(%q) should fail, got %+v", sql, res)
		}
	}
	if res := ro.Run(ctx, "SELECT count(*) FROM explorer_it", 10); res[0].Error != "" || res[0].Rows[0][0].Value != "2" {
		t.Fatalf("read-only select = %+v", res)
	}
	if !strings.HasPrefix(ro.Run(ctx, "SELECT 1", 10)[0].Command, "SELECT") {
		t.Fatal("unexpected command tag")
	}
	estimated, err := ro.Explain(ctx, "SELECT * FROM explorer_it WHERE id = 1", false)
	if err != nil || estimated.Root.NodeType == "" || estimated.Root.ActualRows != nil {
		t.Fatalf("estimated plan = %+v, err = %v", estimated, err)
	}
	actual, err := ro.Explain(ctx, "SELECT * FROM explorer_it WHERE id = 1", true)
	if err != nil || actual.Root.NodeType == "" || actual.Root.ActualRows == nil {
		t.Fatalf("actual plan = %+v, err = %v", actual, err)
	}
	if _, err := ro.Explain(ctx, "DELETE FROM explorer_it", true); err == nil {
		t.Fatal("EXPLAIN ANALYZE mutation should fail in a read-only session")
	}
	if _, err := edit.Explain(ctx, "SELECT 1", false); err == nil {
		t.Fatal("execution plan should require a read-only target")
	}
	edit.Run(ctx, "DROP TABLE explorer_it", 10)
}

func TestVersionedMigrationIntegration(t *testing.T) {
	ctx := context.Background()
	targets := openIsolatedTargets(t, 2)
	firstTarget, secondTarget := targets[0], targets[1]
	suffix := time.Now().UnixNano()
	firstTable := pgx.Identifier{fmt.Sprintf("migration_first_%d", suffix)}.Sanitize()
	secondTable := pgx.Identifier{fmt.Sprintf("migration_second_%d", suffix)}.Sanitize()
	thirdTable := pgx.Identifier{fmt.Sprintf("migration_third_%d", suffix)}.Sanitize()
	t.Cleanup(func() {
		firstTarget.Run(context.Background(), "DROP TABLE IF EXISTS "+thirdTable+"; DROP TABLE IF EXISTS "+secondTable+"; DROP TABLE IF EXISTS "+firstTable, 10)
	})
	first := explorer.Migration{ID: suffix, Version: 1, SQL: "CREATE TABLE " + firstTable + " (id integer); INSERT INTO " + firstTable + " VALUES (1)"}
	results, already, err := firstTarget.ApplyVersionedMigration(ctx, first, 7, 10)
	if err != nil || already || hasError(results) {
		t.Fatalf("first migration: results=%+v already=%v err=%v", results, already, err)
	}
	_, already, err = firstTarget.ApplyVersionedMigration(ctx, first, 7, 10)
	if err != nil || !already {
		t.Fatalf("repeated migration executed: already=%v err=%v", already, err)
	}
	var count int
	if err := firstTarget.conn.QueryRow(ctx, `SELECT count(*) FROM `+firstTable).Scan(&count); err != nil || count != 1 {
		t.Fatalf("first table rows=%d err=%v", count, err)
	}
	second := explorer.Migration{ID: suffix + 1, Version: 2, SQL: "CREATE TABLE " + secondTable + " (id integer); INSERT INTO " + secondTable + " VALUES (1); SELECT 1/0"}
	results, _, err = firstTarget.ApplyVersionedMigration(ctx, second, 7, 10)
	if err != nil || !hasError(results) {
		t.Fatalf("failing migration: results=%+v err=%v", results, err)
	}
	var missing bool
	if err := firstTarget.conn.QueryRow(ctx, `SELECT to_regclass($1) IS NULL`, strings.Trim(secondTable, `"`)).Scan(&missing); err != nil || !missing {
		t.Fatalf("rollback table missing=%v err=%v", missing, err)
	}
	third := explorer.Migration{ID: suffix + 2, Version: 3, SQL: "CREATE TABLE " + thirdTable + " (id integer)"}
	if _, _, err := firstTarget.ApplyVersionedMigration(ctx, third, 7, 10); !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("out-of-order migration: %v", err)
	}
	second.SQL = "CREATE TABLE " + secondTable + " (id integer)"
	if results, _, err := firstTarget.ApplyVersionedMigration(ctx, second, 7, 10); err != nil || hasError(results) {
		t.Fatalf("retry second migration: results=%+v err=%v", results, err)
	}
	type outcome struct {
		already bool
		err     error
	}
	started := make(chan struct{})
	outcomes := make(chan outcome, 2)
	for _, target := range []*pgTarget{firstTarget, secondTarget} {
		go func(target *pgTarget) {
			<-started
			results, already, err := target.ApplyVersionedMigration(ctx, third, 7, 10)
			if hasError(results) {
				err = fmt.Errorf("migration result: %+v", results)
			}
			outcomes <- outcome{already: already, err: err}
		}(target)
	}
	close(started)
	one, two := <-outcomes, <-outcomes
	if one.err != nil || two.err != nil || one.already == two.already {
		t.Fatalf("concurrent migration: first=%+v second=%+v", one, two)
	}
	var versions []int64
	rows, err := firstTarget.conn.Query(ctx, `SELECT version FROM public._dbmp_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	versions, err = pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil || !slices.Equal(versions, []int64{1, 2, 3}) {
		t.Fatalf("ledger versions=%v err=%v", versions, err)
	}
	control := explorer.Migration{ID: suffix + 3, Version: 4, SQL: "COMMIT; CREATE TABLE forbidden_transaction (id integer)"}
	if results, _, err := firstTarget.ApplyVersionedMigration(ctx, control, 7, 10); err != nil || !hasError(results) {
		t.Fatalf("transaction control accepted: results=%+v err=%v", results, err)
	}
}

func TestTargetOptionsRuntimeParams(t *testing.T) {
	cases := map[string]struct {
		opts TargetOptions
		want map[string]string
	}{
		"viewer read-only": {ViewerTargetOptions(true), map[string]string{
			"application_name": "if3110-explorer", "statement_timeout": "10000ms", "default_transaction_read_only": "on",
		}},
		"viewer editor": {ViewerTargetOptions(false), map[string]string{
			"application_name": "if3110-explorer", "statement_timeout": "10000ms",
		}},
		"worker": {WorkerTargetOptions(90 * time.Second), map[string]string{
			"application_name": "if3110-migration-worker", "statement_timeout": "90000ms", "lock_timeout": "10000ms",
		}},
		"worker default timeout": {WorkerTargetOptions(0), map[string]string{
			"application_name": "if3110-migration-worker", "statement_timeout": "300000ms", "lock_timeout": "10000ms",
		}},
		"sub-millisecond rounds up": {TargetOptions{ApplicationName: "x", StatementTimeout: time.Microsecond}, map[string]string{
			"application_name": "x", "statement_timeout": "1ms",
		}},
	}
	for name, c := range cases {
		if got := c.opts.runtimeParams(); !maps.Equal(got, c.want) {
			t.Errorf("%s: params = %v, want %v", name, got, c.want)
		}
	}
	if WorkerTargetOptions(time.Minute).ReadOnly {
		t.Fatal("worker connections must be writable")
	}
}

func TestWorkerTargetOptionsApplyIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	maps.Copy(config.RuntimeParams, WorkerTargetOptions(90*time.Second).runtimeParams())
	target := connectTestTarget(t, config, false)
	ctx := context.Background()
	for setting, want := range map[string]string{
		"statement_timeout": "90s", "lock_timeout": "10s", "application_name": "if3110-migration-worker", "transaction_read_only": "off",
	} {
		var got string
		if err := target.conn.QueryRow(ctx, `SELECT current_setting($1)`, setting).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s = %q, want %q", setting, got, want)
		}
	}
}

func requireTargetSQLState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("err = %v, want SQLSTATE %s", err, code)
	}
}

func TestEnsureBookkeepingIntegration(t *testing.T) {
	target := openIsolatedTargets(t, 1)[0]
	ctx := context.Background()
	for range 2 {
		if err := ensureBookkeeping(ctx, target.conn); err != nil {
			t.Fatal(err)
		}
	}
	var tables int
	if err := target.conn.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'
		AND table_name IN ('_dbmp_migrations', '_dbmp_batch_checkpoints', '_dbmp_batch_backup')`).Scan(&tables); err != nil || tables != 3 {
		t.Fatalf("bookkeeping tables = %d, err = %v", tables, err)
	}

	exec := "11111111-1111-1111-1111-111111111111"
	if _, err := target.conn.Exec(ctx, `
		INSERT INTO public._dbmp_migrations (version, migration_id, sql_text, executed_by, execution_id, status)
		VALUES (1, 10, 'SELECT 1', 1, $1, 'REVERTED')`, exec); err != nil {
		t.Fatalf("widened status and execution_id: %v", err)
	}
	_, err := target.conn.Exec(ctx, `
		INSERT INTO public._dbmp_migrations (version, migration_id, sql_text, executed_by, execution_id)
		VALUES (2, 11, 'SELECT 1', 1, $1)`, exec)
	requireTargetSQLState(t, err, "23505")
	_, err = target.conn.Exec(ctx, `
		INSERT INTO public._dbmp_migrations (version, migration_id, sql_text, executed_by, status)
		VALUES (3, 12, 'SELECT 1', 1, 'BOGUS')`)
	requireTargetSQLState(t, err, "23514")

	checkpoint := `INSERT INTO public._dbmp_batch_checkpoints
		(source_execution_id, batch_no, direction, execution_id, cursor_from, cursor_to, row_count)
		VALUES ($1, 1, $2, $1, 0, 100, 100)`
	if _, err := target.conn.Exec(ctx, checkpoint, exec, "UP"); err != nil {
		t.Fatal(err)
	}
	_, err = target.conn.Exec(ctx, checkpoint, exec, "UP")
	requireTargetSQLState(t, err, "23505")
	if _, err := target.conn.Exec(ctx, checkpoint, exec, "DOWN"); err != nil {
		t.Fatalf("a DOWN checkpoint for the same batch: %v", err)
	}
	_, err = target.conn.Exec(ctx, checkpoint, exec, "SIDEWAYS")
	requireTargetSQLState(t, err, "23514")

	if err := ensureBookkeeping(ctx, target.conn); err != nil {
		t.Fatalf("rerun with data present: %v", err)
	}
	var rows int
	if err := target.conn.QueryRow(ctx, `SELECT count(*) FROM public._dbmp_batch_checkpoints`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("checkpoints after rerun = %d, err = %v", rows, err)
	}
}

func TestEnsureBookkeepingUpgradesTheLegacyLedgerIntegration(t *testing.T) {
	target := openIsolatedTargets(t, 1)[0]
	ctx := context.Background()
	_, applied, err := target.ApplyVersionedMigration(ctx, explorer.Migration{ID: 7, Version: 1, SQL: "CREATE TABLE legacy (id int)"}, 1, 10)
	if err != nil || applied {
		t.Fatalf("legacy apply: applied = %v, err = %v", applied, err)
	}
	if err := ensureBookkeeping(ctx, target.conn); err != nil {
		t.Fatal(err)
	}
	var status string
	var execution *string
	if err := target.conn.QueryRow(ctx, `SELECT status, execution_id::text FROM public._dbmp_migrations WHERE migration_id = 7`).Scan(&status, &execution); err != nil {
		t.Fatal(err)
	}
	if status != "APPLIED" || execution != nil {
		t.Fatalf("legacy row = %s / %v", status, execution)
	}
	if _, applied, err := target.ApplyVersionedMigration(ctx, explorer.Migration{ID: 8, Version: 2, SQL: "SELECT 1"}, 1, 10); err != nil || applied {
		t.Fatalf("legacy apply after the upgrade: applied = %v, err = %v", applied, err)
	}
}

func TestEnsureBookkeepingConcurrentIntegration(t *testing.T) {
	targets := openIsolatedTargets(t, 4)
	var wg sync.WaitGroup
	errs := make(chan error, len(targets))
	for _, target := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- ensureBookkeeping(context.Background(), target.conn)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent bookkeeping: %v", err)
		}
	}
}

func TestInspectBatchTargetIntegration(t *testing.T) {
	target := openIsolatedTargets(t, 1)[0]
	ctx := context.Background()
	if _, err := target.conn.Exec(ctx, `
		CREATE SCHEMA app;
		CREATE TABLE app."Orders" (id bigint PRIMARY KEY, code int NOT NULL UNIQUE, note text);
		CREATE TABLE public.small (id smallint PRIMARY KEY);
		CREATE TABLE public.plain (id int NOT NULL, name text NOT NULL UNIQUE, uid uuid PRIMARY KEY);
		CREATE TABLE public.nullable (id int UNIQUE);
		CREATE TABLE public.composite (a int NOT NULL, b int NOT NULL, PRIMARY KEY (a, b));
		CREATE TABLE public.partial (id int NOT NULL);
		CREATE UNIQUE INDEX partial_id ON public.partial (id) WHERE id > 0;
		CREATE VIEW public.orders_view AS SELECT * FROM app."Orders";
		CREATE TABLE public.dropped (id int PRIMARY KEY, gone int);
		ALTER TABLE public.dropped DROP COLUMN gone;`); err != nil {
		t.Fatal(err)
	}

	for _, ok := range []domain.BatchConfig{
		{Table: "app.Orders", CursorColumn: "id"},
		{Table: "app.Orders", CursorColumn: "code"},
		{Table: "small", CursorColumn: "id"},
		{Table: "public.dropped", CursorColumn: "id"},
	} {
		got, err := inspectBatchTarget(ctx, target.conn, ok)
		if err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
		if got.table == "" || got.column == "" {
			t.Errorf("%+v: identifiers = %+v", ok, got)
		}
	}
	if got, _ := inspectBatchTarget(ctx, target.conn, domain.BatchConfig{Table: "app.Orders", CursorColumn: "id"}); got.table != `"app"."Orders"` || got.column != `"id"` {
		t.Fatalf("quoted identifiers = %+v", got)
	}

	for name, bad := range map[string]domain.BatchConfig{
		"missing table":       {Table: "app.missing", CursorColumn: "id"},
		"missing schema":      {Table: "nope.Orders", CursorColumn: "id"},
		"wrong case":          {Table: "app.orders", CursorColumn: "id"},
		"view":                {Table: "orders_view", CursorColumn: "id"},
		"missing column":      {Table: "app.Orders", CursorColumn: "nope"},
		"text column":         {Table: "plain", CursorColumn: "name"},
		"uuid column":         {Table: "plain", CursorColumn: "uid"},
		"not unique":          {Table: "plain", CursorColumn: "id"},
		"nullable":            {Table: "nullable", CursorColumn: "id"},
		"composite key part":  {Table: "composite", CursorColumn: "a"},
		"partial unique":      {Table: "partial", CursorColumn: "id"},
		"not indexed":         {Table: "app.Orders", CursorColumn: "note"},
		"system column":       {Table: "app.Orders", CursorColumn: "ctid"},
		"malformed":           {Table: "a.b.c", CursorColumn: "id"},
		"injection attempt":   {Table: `app.Orders"; DROP TABLE app."Orders`, CursorColumn: "id"},
		"empty cursor column": {Table: "app.Orders", CursorColumn: ""},
	} {
		if _, err := inspectBatchTarget(ctx, target.conn, bad); !errors.Is(err, migrationengine.ErrInvalidBatch) {
			t.Errorf("%s: err = %v, want invalid batch", name, err)
		}
	}
	var n int
	if err := target.conn.QueryRow(ctx, `SELECT count(*) FROM app."Orders"`).Scan(&n); err != nil {
		t.Fatalf("table survived the injection attempt? %v", err)
	}
}

func TestCommitRejected(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"deferred constraint":  {&pgconn.PgError{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "23503"}, true},
		"localized severity":   {&pgconn.PgError{Severity: "ERROR", Code: "40001"}, true},
		"wrapped error":        {fmt.Errorf("commit: %w", &pgconn.PgError{SeverityUnlocalized: "ERROR", Code: "23505"}), true},
		"terminated backend":   {&pgconn.PgError{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "57P01"}, false},
		"server crash":         {&pgconn.PgError{SeverityUnlocalized: "PANIC", Code: "XX000"}, false},
		"connection dropped":   {io.ErrUnexpectedEOF, false},
		"translated FATAL msg": {&pgconn.PgError{Severity: "FATAL", Code: "57P01"}, false},
	}
	for name, c := range cases {
		if _, got := commitRejected(c.err); got != c.want {
			t.Errorf("%s: commitRejected = %v, want %v", name, got, c.want)
		}
	}
}

const batchSource = "bbbbbbbb-0000-4000-8000-000000000001"

func batchFixture(t *testing.T) (*pgTarget, batchSpec) {
	t.Helper()
	target := openIsolatedTargets(t, 1)[0]
	ctx := context.Background()
	if err := ensureBookkeeping(ctx, target.conn); err != nil {
		t.Fatal(err)
	}
	if _, err := target.conn.Exec(ctx, `
		CREATE TABLE items (id bigint PRIMARY KEY, v int NOT NULL DEFAULT 0);
		INSERT INTO items (id) SELECT g FROM generate_series(1, 12) g;`); err != nil {
		t.Fatal(err)
	}
	bt, err := inspectBatchTarget(ctx, target.conn, domain.BatchConfig{Table: "items", CursorColumn: "id"})
	if err != nil {
		t.Fatal(err)
	}
	return target, batchSpec{
		target: bt, sql: `UPDATE items SET v = v + 1 WHERE id > $1 AND id <= $2`,
		sourceID: batchSource, executionID: batchSource, direction: domain.DirectionUp,
		batchNo: 1, from: 0, scopeMax: 10, size: 4,
	}
}

func itemsChanged(t *testing.T, target *pgTarget) (changed, total int) {
	t.Helper()
	if err := target.conn.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE v > 0), coalesce(sum(v), 0) FROM items`).Scan(&changed, &total); err != nil {
		t.Fatal(err)
	}
	return changed, total
}

func TestRunBatchCommitsOneBatchWithItsCheckpointIntegration(t *testing.T) {
	target, spec := batchFixture(t)
	ctx := context.Background()
	e := &TargetExecutor{}

	out, err := e.runBatch(ctx, target.conn, spec)
	if err != nil || out.done || out.to != 4 || out.rows != 4 {
		t.Fatalf("first batch = %+v, err = %v", out, err)
	}
	var from, to, rows int64
	if err := target.conn.QueryRow(ctx, `
		SELECT cursor_from, cursor_to, row_count FROM public._dbmp_batch_checkpoints
		WHERE source_execution_id = $1 AND batch_no = 1 AND direction = 'UP'`, batchSource).Scan(&from, &to, &rows); err != nil {
		t.Fatal(err)
	}
	if from != 0 || to != 4 || rows != 4 {
		t.Fatalf("checkpoint = (%d, %d] rows %d", from, to, rows)
	}

	_, err = e.runBatch(ctx, target.conn, spec)
	requireTargetSQLState(t, err, "23505")
	if changed, total := itemsChanged(t, target); changed != 4 || total != 4 {
		t.Fatalf("after replaying batch 1: %d rows changed, sum %d; want the data changed once", changed, total)
	}

	spec.batchNo, spec.from = 2, 4
	if out, err := e.runBatch(ctx, target.conn, spec); err != nil || out.to != 8 {
		t.Fatalf("second batch = %+v, err = %v", out, err)
	}
	spec.batchNo, spec.from = 3, 8
	if out, err := e.runBatch(ctx, target.conn, spec); err != nil || out.to != 10 || out.rows != 2 {
		t.Fatalf("last batch stops at the scope: %+v, err = %v", out, err)
	}
	spec.batchNo, spec.from = 4, 10
	if out, err := e.runBatch(ctx, target.conn, spec); err != nil || !out.done {
		t.Fatalf("past the scope = %+v, err = %v; want done", out, err)
	}
	if changed, _ := itemsChanged(t, target); changed != 10 {
		t.Fatalf("%d rows changed; rows 11 and 12 are outside the scope", changed)
	}
}

func TestRunBatchFailureRollsBackOnlyThatBatchIntegration(t *testing.T) {
	target, spec := batchFixture(t)
	ctx := context.Background()
	e := &TargetExecutor{}
	if _, err := e.runBatch(ctx, target.conn, spec); err != nil {
		t.Fatal(err)
	}
	spec.batchNo, spec.from = 2, 4
	spec.sql = `UPDATE items SET v = v + 1 / (id - 6) WHERE id > $1 AND id <= $2`
	_, err := e.runBatch(ctx, target.conn, spec)
	requireTargetSQLState(t, err, "22012")
	if changed, _ := itemsChanged(t, target); changed != 4 {
		t.Fatalf("%d rows changed; only batch 1 should be committed", changed)
	}
	var checkpoints int
	if err := target.conn.QueryRow(ctx, `SELECT count(*) FROM public._dbmp_batch_checkpoints`).Scan(&checkpoints); err != nil || checkpoints != 1 {
		t.Fatalf("checkpoints = %d, err = %v", checkpoints, err)
	}
}

func TestRunBatchLostCommitIsUncertainIntegration(t *testing.T) {
	target, spec := batchFixture(t)
	killer := openTestTarget(t, false)
	e := &TargetExecutor{beforeCommit: func(pid uint32) {
		_, _ = killer.conn.Exec(context.Background(), `SELECT pg_terminate_backend($1)`, int64(pid))
	}}
	_, err := e.runBatch(context.Background(), target.conn, spec)
	if !errors.Is(err, migrationengine.ErrCommitUncertain) {
		t.Fatalf("err = %v, want an uncertain commit", err)
	}
}

func TestRunBatchKeepsABeforeImageIntegration(t *testing.T) {
	target, spec := batchFixture(t)
	ctx := context.Background()
	e := &TargetExecutor{}
	if _, err := target.conn.Exec(ctx, `UPDATE items SET v = id * 7 WHERE id <= 4`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.runBatch(ctx, target.conn, spec); err != nil {
		t.Fatal(err)
	}
	var mismatched, backed int
	if err := target.conn.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE (b.before_row->>'v')::int <> b.row_key * 7 OR (b.before_row->>'id')::bigint <> b.row_key),
		       count(*)
		FROM public._dbmp_batch_backup b
		WHERE b.source_execution_id = $1 AND b.batch_no = 1`, batchSource).Scan(&mismatched, &backed); err != nil {
		t.Fatal(err)
	}
	if backed != 4 || mismatched != 0 {
		t.Fatalf("backup rows = %d, %d not matching the pre-change data", backed, mismatched)
	}
	var after int
	if err := target.conn.QueryRow(ctx, `SELECT v FROM items WHERE id = 2`).Scan(&after); err != nil || after != 15 {
		t.Fatalf("row 2 after the batch = %d, err = %v; the backup must hold the old value 14", after, err)
	}

	spec.batchNo, spec.from = 2, 4
	spec.sql = `UPDATE items SET v = v + 1 / (id - 6) WHERE id > $1 AND id <= $2`
	if _, err := e.runBatch(ctx, target.conn, spec); err == nil {
		t.Fatal("batch 2 should fail")
	}
	var orphans int
	if err := target.conn.QueryRow(ctx, `SELECT count(*) FROM public._dbmp_batch_backup WHERE batch_no = 2`).Scan(&orphans); err != nil || orphans != 0 {
		t.Fatalf("a failed batch left %d backup rows", orphans)
	}

	down := spec
	down.batchNo, down.from, down.direction = 1, 0, domain.DirectionDown
	down.executionID = "bbbbbbbb-0000-4000-8000-000000000002"
	down.sql = `UPDATE items SET v = v - 1 WHERE id > $1 AND id <= $2`
	if _, err := e.runBatch(ctx, target.conn, down); err != nil {
		t.Fatal(err)
	}
	if err := target.conn.QueryRow(ctx, `SELECT count(*) FROM public._dbmp_batch_backup`).Scan(&backed); err != nil || backed != 4 {
		t.Fatalf("a DOWN batch must not write backup rows: %d, err = %v", backed, err)
	}
}
