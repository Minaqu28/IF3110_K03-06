package database

import (
	"context"
	"maps"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

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
