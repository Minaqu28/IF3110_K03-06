// Command k6seed prepares approved migrations across two target databases ("shop"
// and "analytics") and writes their IDs to k6/migrations.json for k6 stress
// testing.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
	"github.com/Minaqu28/IF3110_K03-06/internal/migration"
	"github.com/Minaqu28/IF3110_K03-06/internal/workflow"
	"github.com/jackc/pgx/v5"
)

type seededMigration struct {
	DatabaseID        int64  `json:"database_id"`
	DatabaseName      string `json:"database_name"`
	MigrationID       int64  `json:"migration_id"`
	RevisionID        int64  `json:"revision_id"`
	ApprovalRequestID int64  `json:"approval_request_id"`
	Action            string `json:"action"`
	Mode              string `json:"mode"`
}

type seedManifest struct {
	GeneratedAt string                       `json:"generated_at"`
	Databases   map[string][]seededMigration `json:"databases"`
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dsn := mustEnv("DATABASE_URL")
	credKey := mustEnv("DB_CREDENTIAL_KEY")
	poolSize := envInt("K6_MIGRATION_POOL", 150)
	outPath := getenv("K6_MANIFEST_PATH", filepath.Join("k6", "migrations.json"))
	adminUser := getenv("BOOTSTRAP_ADMIN_USERNAME", "admin")

	db, err := database.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := database.Migrate(ctx, db, migration.Files); err != nil {
		return err
	}

	adminStore, err := database.NewAdminPostgresStore(db, credKey)
	if err != nil {
		return err
	}
	revisions := database.NewRevisionPostgresStore(db)
	approvals := database.NewApprovalPostgresStore(db)

	var actorID int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM users WHERE username = $1`, adminUser).Scan(&actorID); err != nil {
		return fmt.Errorf("find user %q (run /seed first): %w", adminUser, err)
	}

	conns, err := adminStore.ListConnections(ctx)
	if err != nil {
		return err
	}
	connByName := map[string]domain.Connection{}
	for _, c := range conns {
		connByName[c.Name] = c
	}

	// Ensure the bootstrap admin belongs to a VIEW_EDIT group with a leader on each target DB
	if err := ensureAdminAccess(ctx, db, adminStore, actorID, conns); err != nil {
		return err
	}

	manifest := seedManifest{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Databases:   map[string][]seededMigration{},
	}

	for _, dbName := range []string{"shop", "analytics"} {
		conn, ok := connByName[dbName]
		if !ok {
			return fmt.Errorf("connection %q not found (run /seed first)", dbName)
		}
		if err := ensureStressTable(ctx, adminStore, conn.ID); err != nil {
			return fmt.Errorf("prepare target table on %s: %w", dbName, err)
		}
		items, err := seedApprovedMigrations(ctx, db, adminStore, revisions, approvals, conn, actorID, poolSize)
		if err != nil {
			return fmt.Errorf("seed approved migrations on %s: %w", dbName, err)
		}
		manifest.Databases[dbName] = items
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(outPath, raw, 0o644); err != nil {
		return err
	}
	log.Printf("wrote %d approved migrations per database to %s", poolSize, outPath)
	return nil
}

func ensureAdminAccess(ctx context.Context, db *sql.DB, store *database.AdminPostgresStore, actorID int64, conns []domain.Connection) error {
	var groupID int64
	err := db.QueryRowContext(ctx, `SELECT id FROM groups WHERE name = 'k6-testers'`).Scan(&groupID)
	if err == sql.ErrNoRows {
		g, err := store.CreateGroup(ctx, "k6-testers")
		if err != nil {
			return err
		}
		groupID = g.ID
	} else if err != nil {
		return err
	}
	if err := store.AddMember(ctx, groupID, actorID); err != nil && err != admin.ErrConflict {
		return err
	}
	if err := store.SetGroupLeader(ctx, groupID, &actorID); err != nil {
		return err
	}
	for _, c := range conns {
		if err := store.GrantAccess(ctx, groupID, c.ID, authorization.AccessViewEdit); err != nil {
			return err
		}
	}
	return nil
}

func ensureStressTable(ctx context.Context, store *database.AdminPostgresStore, dbID int64) error {
	target, err := store.OpenTarget(ctx, dbID, false)
	if err != nil {
		return err
	}
	defer target.Close(context.WithoutCancel(ctx))
	res := target.Run(ctx, `
		CREATE TABLE IF NOT EXISTS public.k6_stress_log (
			id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			label      TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`, 10)
	for _, r := range res {
		if r.Error != "" {
			return fmt.Errorf("target sql error: %s", r.Error)
		}
	}
	return nil
}

func seedApprovedMigrations(
	ctx context.Context,
	db *sql.DB,
	adminStore *database.AdminPostgresStore,
	revisions *database.RevisionPostgresStore,
	approvals *database.ApprovalPostgresStore,
	conn domain.Connection,
	actorID int64,
	count int,
) ([]seededMigration, error) {
	grants, err := approvals.ListGroupGrants(ctx, conn.ID)
	if err != nil {
		return nil, err
	}
	approvers, err := workflow.RequiredGroups(conn.ID, grants)
	if err != nil {
		return nil, err
	}

	runTag := time.Now().UTC().Format("20060102T150405")
	out := make([]seededMigration, 0, count)

	// Avoid unused import if pgx isn't directly called
	_ = pgx.Identifier{}

	for i := 1; i <= count; i++ {
		label := fmt.Sprintf("k6-%s-%s-%04d", conn.Name, runTag, i)
		upSQL := fmt.Sprintf("INSERT INTO public.k6_stress_log (label) VALUES ('%s')", label)
		downSQL := fmt.Sprintf("DELETE FROM public.k6_stress_log WHERE label = '%s'", label)

		m, err := adminStore.CreateMigration(ctx, explorer.MigrationInput{
			DatabaseID: conn.ID,
			UserID:     actorID,
			Title:      fmt.Sprintf("k6 stress %s #%d", conn.Name, i),
			SQL:        upSQL,
		})
		if err != nil {
			return nil, err
		}

		rev, err := revisions.CreateRevision(ctx, domain.MigrationRevision{
			MigrationID: m.ID,
			DatabaseID:  conn.ID,
			Title:       m.Title,
			UpSQL:       upSQL,
			DownSQL:     downSQL,
			Reversible:  true,
			CreatedBy:   actorID,
		})
		if err != nil {
			return nil, err
		}

		review, err := approvals.SubmitActionForReview(ctx, conn.ID, rev.ID, actorID, domain.MigrationActionApply, nil, approvers)
		if err != nil {
			return nil, err
		}

		for _, a := range approvers {
			if _, err := approvals.DecideReview(ctx, review.ID, a.LeaderID, domain.ApprovalStatusApproved, "k6 pre-approved", workflow.ResolveReview); err != nil {
				return nil, err
			}
		}

		out = append(out, seededMigration{
			DatabaseID:        conn.ID,
			DatabaseName:      conn.Name,
			MigrationID:       m.ID,
			RevisionID:        rev.ID,
			ApprovalRequestID: review.ID,
			Action:            string(domain.MigrationActionApply),
			Mode:              string(domain.ExecutionModeNormal),
		})
	}
	_ = db
	return out, nil
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("%s is required", key)
	}
	return v
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		log.Fatalf("%s must be a positive integer, got %q", key, v)
	}
	return n
}
