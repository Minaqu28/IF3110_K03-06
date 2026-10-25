package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
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
	firstTarget := openTestTarget(t, false)
	secondTarget := openTestTarget(t, false)
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
