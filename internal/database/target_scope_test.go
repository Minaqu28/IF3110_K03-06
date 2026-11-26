package database_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
	"github.com/jackc/pgx/v5"
)

func TestBatchScopePostgres(t *testing.T) {
	rootDSN := os.Getenv("TEST_POSTGRES_DSN")
	if rootDSN == "" {
		t.Skip("set TEST_POSTGRES_DSN to run target integration")
	}
	ctx := context.Background()
	root, err := pgx.Connect(ctx, rootDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("dbmp_scope_%d", time.Now().UnixNano())
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := root.Exec(ctx, `CREATE DATABASE `+ident); err != nil {
		root.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = root.Exec(context.Background(), `DROP DATABASE `+ident+` WITH (FORCE)`)
		_ = root.Close(context.Background())
	})
	config, err := pgx.ParseConfig(rootDSN)
	if err != nil {
		t.Fatal(err)
	}
	config.Database = name
	target, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.Exec(ctx, `
		CREATE SCHEMA app;
		CREATE TABLE app."Orders" (id bigint PRIMARY KEY);
		INSERT INTO app."Orders" SELECT g FROM generate_series(5, 20) g;
		CREATE TABLE public.empty (id bigint PRIMARY KEY);`); err != nil {
		t.Fatal(err)
	}
	_ = target.Close(ctx)

	platform := integrationDB(t)
	store, err := database.NewAdminPostgresStore(platform, newCredentialKey(t))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := store.CreateConnection(ctx, domain.Connection{
		Name: "scope target", Host: config.Host, Port: int64(config.Port), DBname: name, DBuser: config.User, SSLMode: "disable",
	}, config.Password)
	if err != nil {
		t.Fatal(err)
	}

	scope, err := store.BatchScope(ctx, conn.ID, domain.BatchConfig{Table: "app.Orders", CursorColumn: "id", BatchSize: 5})
	if err != nil || scope != (workflow.BatchScope{Min: 4, Max: 20}) {
		t.Fatalf("scope = %+v, err = %v; want 4..20", scope, err)
	}
	empty, err := store.BatchScope(ctx, conn.ID, domain.BatchConfig{Table: "empty", CursorColumn: "id", BatchSize: 5})
	if err != nil || empty != (workflow.BatchScope{}) {
		t.Fatalf("empty table scope = %+v, err = %v; want 0..0", empty, err)
	}
	for _, bad := range []domain.BatchConfig{
		{Table: "app.missing", CursorColumn: "id"},
		{Table: "app.Orders", CursorColumn: "nope"},
		{Table: "a.b.c", CursorColumn: "id"},
		{Table: "app.", CursorColumn: "id"},
		{Table: "app.Orders", CursorColumn: ""},
		{Table: `app.Orders"; DROP TABLE app."Orders`, CursorColumn: "id"},
	} {
		if _, err := store.BatchScope(ctx, conn.ID, bad); !errors.Is(err, admin.ErrInvalidInput) {
			t.Errorf("BatchScope(%+v): err = %v, want invalid input", bad, err)
		}
	}
	if scope, err := store.BatchScope(ctx, conn.ID, domain.BatchConfig{Table: "app.Orders", CursorColumn: "id"}); err != nil || scope.Max != 20 {
		t.Fatalf("table survived the injection attempt? scope = %+v, err = %v", scope, err)
	}
}
