// Command seed fills the platform with demo data: users, groups, database
// connections, access grants, and sample tables with deterministic rows in the
// target databases. It is idempotent: running it again changes nothing.
//
// Run inside compose (see README):
//
//	docker compose run --rm --entrypoint /seed app
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authentication"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/database"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/migration"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

type seedUser struct {
	name     string
	admin    bool
	inactive bool
	groups   []string
	note     string
}

var users = []seedUser{
	{name: "root2", admin: true, note: "second platform admin"},
	{name: "user1", groups: []string{"developers"}, note: "developer: View + Edit on shop"},
	{name: "user2", groups: []string{"analysts"}, note: "analyst: View Only on shop, View + Edit on analytics"},
	{name: "user3", groups: []string{"developers", "analysts"}, note: "in two groups: highest grant wins"},
	{name: "user4", note: "no group, so no database access"},
	{name: "user5", inactive: true, groups: []string{"developers"}, note: "inactive: cannot log in"},
}

var groups = []string{"developers", "analysts", "auditors"}

type grant struct {
	group, database string
	level           authorization.AccessLevel
}

var grants = []grant{
	{"developers", "shop", authorization.AccessViewEdit},
	{"developers", "analytics", authorization.AccessViewOnly},
	{"analysts", "shop", authorization.AccessViewOnly},
	{"analysts", "analytics", authorization.AccessViewEdit},
	{"auditors", "shop", authorization.AccessViewOnly},
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	platformURL := mustEnv("DATABASE_URL")
	credKey := mustEnv("DB_CREDENTIAL_KEY")
	targetPassword := mustEnv("TARGET_DB_PASSWORD")
	userPassword := getenv("SEED_USER_PASSWORD", "aaaaaaaaaaaa")
	if len(userPassword) < 12 {
		return errors.New("SEED_USER_PASSWORD must be at least 12 characters")
	}
	host := getenv("SEED_TARGET_HOST", "target-db")
	connectHost := getenv("SEED_TARGET_CONNECT_HOST", host)
	port := getenv("SEED_TARGET_PORT", "5432")
	targetUser := getenv("SEED_TARGET_USER", "target")

	db, err := database.Open(ctx, platformURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := database.Migrate(ctx, db, migration.Files); err != nil {
		return err
	}
	if err := database.BootstrapAdmin(ctx, db, os.Getenv("BOOTSTRAP_ADMIN_USERNAME"), os.Getenv("BOOTSTRAP_ADMIN_PASSWORD")); err != nil {
		return err
	}
	store, err := database.NewAdminPostgresStore(db, credKey)
	if err != nil {
		return err
	}

	// 1. Sample data in the target server (shop = existing "target" db, plus analytics).
	targetURL := func(dbName string) string {
		return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
			url.QueryEscape(targetUser), url.QueryEscape(targetPassword), connectHost, port, url.PathEscape(dbName))
	}
	if err := ensureDatabase(ctx, targetURL("postgres"), "analytics"); err != nil {
		return err
	}
	if err := seedTarget(ctx, targetURL("target"), seedShop); err != nil {
		return fmt.Errorf("seed shop: %w", err)
	}
	if err := seedTarget(ctx, targetURL("analytics"), seedAnalytics); err != nil {
		return fmt.Errorf("seed analytics: %w", err)
	}

	// 2. Users.
	hash, err := authentication.HashPassword(userPassword)
	if err != nil {
		return err
	}
	existing, err := store.ListUsers(ctx)
	if err != nil {
		return err
	}
	userIDs := map[string]int64{}
	for _, u := range existing {
		userIDs[u.Username] = u.ID
	}
	for _, u := range users {
		if _, ok := userIDs[u.name]; ok {
			continue
		}
		created, err := store.CreateUser(ctx, u.name, hash, u.admin)
		if err != nil {
			return fmt.Errorf("create user %s: %w", u.name, err)
		}
		userIDs[u.name] = created.ID
	}

	// 3. Groups and memberships.
	existingGroups, err := store.ListGroups(ctx)
	if err != nil {
		return err
	}
	groupIDs := map[string]int64{}
	for _, g := range existingGroups {
		groupIDs[g.Name] = g.ID
	}
	for _, name := range groups {
		if _, ok := groupIDs[name]; ok {
			continue
		}
		g, err := store.CreateGroup(ctx, name)
		if err != nil {
			return fmt.Errorf("create group %s: %w", name, err)
		}
		groupIDs[name] = g.ID
	}
	for _, u := range users {
		for _, g := range u.groups {
			if err := store.AddMember(ctx, groupIDs[g], userIDs[u.name]); err != nil && !errors.Is(err, admin.ErrConflict) {
				return fmt.Errorf("add %s to %s: %w", u.name, g, err)
			}
		}
	}
	for _, u := range users {
		if u.inactive {
			if err := store.SetUserActive(ctx, userIDs[u.name], false); err != nil {
				return err
			}
		}
	}

	// 4. Connections (the password is encrypted by the store, as in the admin UI).
	existingConns, err := store.ListConnections(ctx)
	if err != nil {
		return err
	}
	connIDs := map[string]int64{}
	for _, c := range existingConns {
		connIDs[c.Name] = c.ID
	}
	portNum := int64(5432)
	if _, err := fmt.Sscanf(port, "%d", &portNum); err != nil {
		return fmt.Errorf("invalid SEED_TARGET_PORT %q", port)
	}
	for _, spec := range []struct{ name, dbName string }{{"shop", "target"}, {"analytics", "analytics"}} {
		name, dbName := spec.name, spec.dbName
		if _, ok := connIDs[name]; ok {
			continue
		}
		c, err := store.CreateConnection(ctx, domain.Connection{
			Name: name, Host: host, Port: portNum, DBname: dbName, DBuser: targetUser, SSLMode: "disable",
		}, targetPassword)
		if err != nil {
			return fmt.Errorf("create connection %s: %w", name, err)
		}
		connIDs[name] = c.ID
	}

	// 5. Access grants (upsert).
	for _, g := range grants {
		if err := store.GrantAccess(ctx, groupIDs[g.group], connIDs[g.database], g.level); err != nil {
			return fmt.Errorf("grant %s on %s: %w", g.group, g.database, err)
		}
	}

	printSummary(userPassword)
	return nil
}

func printSummary(password string) {
	fmt.Println("\nSeed complete. All seeded users share the password:", password)
	fmt.Printf("\n%-8s %-60s\n", "USER", "WHAT IT DEMONSTRATES")
	for _, u := range users {
		fmt.Printf("%-8s %s\n", u.name, u.note)
	}
	fmt.Println("\nAccess (group -> database: level)")
	for _, g := range grants {
		fmt.Printf("  %-11s -> %-10s %s\n", g.group, g.database, g.level)
	}
	fmt.Println("\nThe bootstrap admin from .env can also log in.")
}

// ---------- target database seeding ----------

func ensureDatabase(ctx context.Context, adminURL, name string) error {
	db, err := database.Open(ctx, adminURL)
	if err != nil {
		return err
	}
	defer db.Close()
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = db.ExecContext(ctx, `CREATE DATABASE `+quoteIdent(name))
	return err
}

func seedTarget(ctx context.Context, dsn string, fill func(context.Context, *sql.Tx) error) error {
	db, err := database.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fill(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func isEmpty(ctx context.Context, tx *sql.Tx, table string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n)
	return n == 0, err
}

var (
	baseTime   = time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	firstNames = []string{"Ayu", "Budi", "Citra", "Dimas", "Eka", "Fajar", "Gita", "Hadi", "Intan", "Joko", "Kirana", "Lukman"}
	lastNames  = []string{"Pratama", "Santoso", "Wijaya", "Lestari", "Hidayat", "Saputra", "Kusuma", "Nugroho"}
	cities     = []string{"Bandung", "Jakarta", "Surabaya", "Yogyakarta", "Medan", "Semarang", "Denpasar"}
	categories = []string{"stationery", "electronics", "books", "home", "toys"}
	statuses   = []string{"pending", "paid", "shipped", "delivered", "cancelled"}
	pages      = []string{"/", "/products", "/cart", "/checkout", "/about", "/search"}
)

func seedShop(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS customers (
			id         SERIAL PRIMARY KEY,
			name       TEXT NOT NULL,
			email      TEXT NOT NULL UNIQUE,
			city       TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL
		);
		CREATE TABLE IF NOT EXISTS products (
			id       SERIAL PRIMARY KEY,
			name     TEXT NOT NULL,
			category TEXT NOT NULL,
			price    NUMERIC(10,2) NOT NULL CHECK (price >= 0),
			stock    INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE IF NOT EXISTS orders (
			id          SERIAL PRIMARY KEY,
			customer_id INTEGER NOT NULL REFERENCES customers(id),
			status      TEXT NOT NULL,
			ordered_at  TIMESTAMPTZ NOT NULL
		);
		CREATE TABLE IF NOT EXISTS order_items (
			order_id   INTEGER NOT NULL REFERENCES orders(id),
			product_id INTEGER NOT NULL REFERENCES products(id),
			quantity   INTEGER NOT NULL CHECK (quantity > 0),
			PRIMARY KEY (order_id, product_id)
		);
		CREATE INDEX IF NOT EXISTS orders_customer_idx ON orders (customer_id);`); err != nil {
		return err
	}
	empty, err := isEmpty(ctx, tx, "customers")
	if err != nil || !empty {
		return err
	}
	rng := rand.New(rand.NewSource(42)) // fixed seed: identical data on every machine

	const nCustomers, nProducts, nOrders = 60, 25, 200
	for i := 1; i <= nCustomers; i++ {
		first, last := firstNames[rng.Intn(len(firstNames))], lastNames[rng.Intn(len(lastNames))]
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO customers (name, email, city, created_at) VALUES ($1, $2, $3, $4)`,
			first+" "+last, fmt.Sprintf("%s.%s%d@example.com", strings.ToLower(first), strings.ToLower(last), i),
			cities[rng.Intn(len(cities))], baseTime.Add(time.Duration(i)*6*time.Hour)); err != nil {
			return err
		}
	}
	for i := 1; i <= nProducts; i++ {
		cat := categories[rng.Intn(len(categories))]
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO products (name, category, price, stock) VALUES ($1, $2, $3, $4)`,
			fmt.Sprintf("%s item %02d", cat, i), cat, float64(rng.Intn(49000)+1000)/100, rng.Intn(200)); err != nil {
			return err
		}
	}
	for i := 1; i <= nOrders; i++ {
		var id int
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO orders (customer_id, status, ordered_at) VALUES ($1, $2, $3) RETURNING id`,
			rng.Intn(nCustomers)+1, statuses[rng.Intn(len(statuses))],
			baseTime.Add(time.Duration(i)*3*time.Hour)).Scan(&id); err != nil {
			return err
		}
		picked := map[int]bool{}
		for n := rng.Intn(3) + 1; n > 0; n-- {
			p := rng.Intn(nProducts) + 1
			if picked[p] {
				continue
			}
			picked[p] = true
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO order_items (order_id, product_id, quantity) VALUES ($1, $2, $3)`,
				id, p, rng.Intn(4)+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func seedAnalytics(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS page_views (
			id         SERIAL PRIMARY KEY,
			path       TEXT NOT NULL,
			visitor_id INTEGER NOT NULL,
			viewed_at  TIMESTAMPTZ NOT NULL
		);
		CREATE TABLE IF NOT EXISTS daily_stats (
			day      DATE PRIMARY KEY,
			visitors INTEGER NOT NULL,
			orders   INTEGER NOT NULL
		);`); err != nil {
		return err
	}
	empty, err := isEmpty(ctx, tx, "page_views")
	if err != nil || !empty {
		return err
	}
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 500; i++ {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO page_views (path, visitor_id, viewed_at) VALUES ($1, $2, $3)`,
			pages[rng.Intn(len(pages))], rng.Intn(120)+1, baseTime.Add(time.Duration(i)*47*time.Minute)); err != nil {
			return err
		}
	}
	for d := 0; d < 60; d++ {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO daily_stats (day, visitors, orders) VALUES ($1, $2, $3)`,
			baseTime.AddDate(0, 0, d), rng.Intn(300)+50, rng.Intn(40)); err != nil {
			return err
		}
	}
	return nil
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
