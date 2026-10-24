package database

import (
	"context"
	"os"
	"strings"
	"testing"

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
	edit.Run(ctx, "DROP TABLE explorer_it", 10)
}
