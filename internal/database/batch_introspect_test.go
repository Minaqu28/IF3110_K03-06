package database

import (
	"context"
	"errors"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migrationengine"
)

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
