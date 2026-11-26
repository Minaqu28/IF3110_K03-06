package domain

import (
	"testing"
	"time"
)

func baseRevision() MigrationRevision {
	return MigrationRevision{
		UpSQL:      "UPDATE public.orders SET total_cents = round(total * 100) WHERE id > $1 AND id <= $2",
		DownSQL:    "UPDATE public.orders SET total_cents = NULL WHERE id > $1 AND id <= $2",
		Reversible: true,
		Batch:      &BatchConfig{Table: "public.orders", CursorColumn: "id", BatchSize: 1000},
	}
}

func TestComputeDefinitionHashIsDeterministic(t *testing.T) {
	a, b := ComputeDefinitionHash(baseRevision()), ComputeDefinitionHash(baseRevision())
	if a != b {
		t.Fatalf("same input gave %s and %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("hash length = %d, want 64 hex chars", len(a))
	}
}

// The golden value guards the encoding: if it changes, every stored approval
// stops matching. Change it only together with definitionHashVersion.
func TestComputeDefinitionHashGolden(t *testing.T) {
	const want = "755deee17069ae2c461058a498117f6225da97afd4b88dbb5d95deba010f77d4"
	if got := ComputeDefinitionHash(baseRevision()); got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
}

func TestComputeDefinitionHashChangesWithDefinition(t *testing.T) {
	base := ComputeDefinitionHash(baseRevision())
	cases := map[string]func(*MigrationRevision){
		"up sql":        func(r *MigrationRevision) { r.UpSQL += " " },
		"down sql":      func(r *MigrationRevision) { r.DownSQL = "" },
		"reversible":    func(r *MigrationRevision) { r.Reversible = false },
		"normal mode":   func(r *MigrationRevision) { r.Batch = nil },
		"batch table":   func(r *MigrationRevision) { r.Batch.Table = "public.order_items" },
		"cursor column": func(r *MigrationRevision) { r.Batch.CursorColumn = "order_id" },
		"batch size":    func(r *MigrationRevision) { r.Batch.BatchSize = 500 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			rev := baseRevision()
			mutate(&rev)
			if ComputeDefinitionHash(rev) == base {
				t.Fatalf("changing %s did not change the hash", name)
			}
		})
	}
}

func TestComputeDefinitionHashIgnoresMetadata(t *testing.T) {
	base := ComputeDefinitionHash(baseRevision())
	now := time.Now()
	rev := baseRevision()
	rev.ID, rev.MigrationID, rev.DatabaseID, rev.Revision = 1, 2, 3, 4
	rev.Title = "backfill total_cents"
	rev.IrreversibleReason = "n/a"
	rev.Status = RevisionStatusApproved
	rev.DefinitionHash = "stale"
	rev.CreatedBy, rev.CreatedAt, rev.LockedAt = 9, now, &now
	if got := ComputeDefinitionHash(rev); got != base {
		t.Fatalf("metadata changed the hash: %s != %s", got, base)
	}
}

func TestComputeDefinitionHashFieldBoundaries(t *testing.T) {
	a := MigrationRevision{UpSQL: "ab", DownSQL: "c"}
	b := MigrationRevision{UpSQL: "a", DownSQL: "bc"}
	if ComputeDefinitionHash(a) == ComputeDefinitionHash(b) {
		t.Fatal("moving text between fields did not change the hash")
	}
}

func TestComputeDefinitionHashEmptyBatchIsNotNormal(t *testing.T) {
	normal := MigrationRevision{UpSQL: "SELECT 1"}
	batch := MigrationRevision{UpSQL: "SELECT 1", Batch: &BatchConfig{}}
	if ComputeDefinitionHash(normal) == ComputeDefinitionHash(batch) {
		t.Fatal("an empty batch config hashed the same as normal mode")
	}
}
