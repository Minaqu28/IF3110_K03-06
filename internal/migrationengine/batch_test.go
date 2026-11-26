package migrationengine

import (
	"errors"
	"testing"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

const (
	goodUp   = `UPDATE public.orders SET total_cents = round(total * 100)::bigint WHERE id > $1 AND id <= $2`
	goodDown = `UPDATE public.orders SET total_cents = NULL WHERE id > $1 AND id <= $2;`
)

func batchRevision(mutate func(*domain.MigrationRevision)) domain.MigrationRevision {
	rev := domain.MigrationRevision{
		UpSQL: goodUp, DownSQL: goodDown, Reversible: true,
		Batch: &domain.BatchConfig{Table: "public.orders", CursorColumn: "id", BatchSize: 1000},
	}
	if mutate != nil {
		mutate(&rev)
	}
	return rev
}

func TestValidateBatchAccepts(t *testing.T) {
	cases := map[string]func(*domain.MigrationRevision){
		"schema-qualified":      nil,
		"bare table":            func(r *domain.MigrationRevision) { r.Batch.Table = "orders" },
		"smallest batch":        func(r *domain.MigrationRevision) { r.Batch.BatchSize = MinBatchSize },
		"largest batch":         func(r *domain.MigrationRevision) { r.Batch.BatchSize = MaxBatchSize },
		"irreversible, no down": func(r *domain.MigrationRevision) { r.Reversible, r.DownSQL = false, "" },
		"bounds used twice": func(r *domain.MigrationRevision) {
			r.UpSQL = `UPDATE t SET a = 1 WHERE id > $1 AND id <= $2 AND $2 > $1`
		},
		"leading comment":        func(r *domain.MigrationRevision) { r.UpSQL = "-- backfill\n" + goodUp },
		"dollar-quoted function": func(r *domain.MigrationRevision) { r.UpSQL = `UPDATE t SET a = $$x$$ WHERE id > $1 AND id <= $2` },
	}
	for name, mutate := range cases {
		if err := ValidateBatch(batchRevision(mutate)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestValidateBatchRejects(t *testing.T) {
	cases := map[string]func(*domain.MigrationRevision){
		"no batch config":         func(r *domain.MigrationRevision) { r.Batch = nil },
		"no table":                func(r *domain.MigrationRevision) { r.Batch.Table = "" },
		"three-part table":        func(r *domain.MigrationRevision) { r.Batch.Table = "db.public.orders" },
		"empty schema":            func(r *domain.MigrationRevision) { r.Batch.Table = ".orders" },
		"no cursor":               func(r *domain.MigrationRevision) { r.Batch.CursorColumn = " " },
		"qualified cursor":        func(r *domain.MigrationRevision) { r.Batch.CursorColumn = "orders.id" },
		"batch size zero":         func(r *domain.MigrationRevision) { r.Batch.BatchSize = 0 },
		"batch size too big":      func(r *domain.MigrationRevision) { r.Batch.BatchSize = MaxBatchSize + 1 },
		"two up statements":       func(r *domain.MigrationRevision) { r.UpSQL = goodUp + "; " + goodUp },
		"empty up":                func(r *domain.MigrationRevision) { r.UpSQL = "  " },
		"up without $2":           func(r *domain.MigrationRevision) { r.UpSQL = `UPDATE t SET a = 1 WHERE id > $1` },
		"up without bounds":       func(r *domain.MigrationRevision) { r.UpSQL = `UPDATE t SET a = 1` },
		"up with $3":              func(r *domain.MigrationRevision) { r.UpSQL = `UPDATE t SET a = $3 WHERE id > $1 AND id <= $2` },
		"up with $10":             func(r *domain.MigrationRevision) { r.UpSQL = `UPDATE t SET a = $10 WHERE id > $1 AND id <= $2` },
		"transaction control":     func(r *domain.MigrationRevision) { r.UpSQL = `COMMIT` },
		"reversible without down": func(r *domain.MigrationRevision) { r.DownSQL = "" },
		"down without bounds":     func(r *domain.MigrationRevision) { r.DownSQL = `UPDATE t SET a = NULL` },
		"two down statements":     func(r *domain.MigrationRevision) { r.DownSQL = goodDown + goodDown },
	}
	for name, mutate := range cases {
		err := ValidateBatch(batchRevision(mutate))
		if !errors.Is(err, ErrInvalidBatch) || !errors.Is(err, admin.ErrInvalidInput) {
			t.Errorf("%s: err = %v, want an invalid batch error", name, err)
		}
	}
}
