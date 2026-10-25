// Package explorer implements the per-database table viewer, SQL editor, and migration manager.
// It enforces access levels: VIEW_ONLY may only read and view migration history/details,
// while VIEW_EDIT may mutate and create, edit, and execute migrations.
package explorer

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/authorization"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

const (
	PreviewLimit  = 50
	MaxResultRows = 200
	MaxSQLBytes   = 64 << 10
	historyLimit  = 50
)

// ErrReadOnly is returned when a VIEW_ONLY user submits a mutating statement or migration action.
var ErrReadOnly = errors.New("read-only access: only SELECT/WITH/SHOW/EXPLAIN/VALUES/TABLE statements are allowed")

// ErrTableNotFound is returned when the requested table is not in the selected schema.
var ErrTableNotFound = errors.New("table not found")

// ErrAuditFailed means target execution may have succeeded, but the platform
// could not persist its result. A retry is safe because the target ledger is checked.
var ErrAuditFailed = errors.New("migration audit failed")

type Table struct {
	Schema string
	Name   string
	Type   string
	Rows   int64 // -1 when the row count could not be determined
}

type Column struct {
	Name     string
	Type     string
	Nullable bool
	Default  string
}

type Constraint struct {
	Name       string
	Type       string
	Definition string
}

type Index struct {
	Name       string
	Definition string
}

type TableDetails struct {
	Columns     []Column
	Constraints []Constraint
	Indexes     []Index
}

type Cell struct {
	Value string
	Null  bool
}

// StatementResult is the outcome of a single SQL statement.
type StatementResult struct {
	Command   string
	Columns   []string
	Rows      [][]Cell
	Truncated bool
	Error     string
}

// Migration represents a versioned database migration in the platform store.
type Migration struct {
	ID         int64
	DatabaseID int64
	Version    int64
	Title      string
	SQL        string
	Operations []string
	Status     string // DRAFT, SUCCESS, FAILED
	Error      string
	DurationMs int64
	CreatedBy  string
	CreatedAt  time.Time
	ExecutedBy string
	ExecutedAt time.Time
}

type MigrationInput struct {
	DatabaseID int64
	UserID     int64
	Version    int64
	Title      string
	SQL        string
}

type MigrationExecution struct {
	DatabaseID int64
	Migration  Migration
	UserID     int64
	Status     string // SUCCESS or FAILED
	Error      string
	DurationMs int64
}

type MigrationRecord struct {
	DatabaseID int64
	UserID     int64
	SQL        string
	Status     string // SUCCESS or FAILED
	Error      string
	DurationMs int64
}

// ChangeLog represents an entry in the database change log.
type ChangeLog struct {
	ID               int64
	MigrationVersion int64
	Source           string // MIGRATION or SQL_EDITOR
	SQL              string
	Status           string // SUCCESS or FAILED
	Error            string
	DurationMs       int64
	Username         string
	CreatedAt        time.Time
}

// ConnectError describes why a target database could not be reached. Reason is
// safe to show to users (it never contains credentials).
type ConnectError struct {
	Reason string
	Err    error
}

func (e *ConnectError) Error() string { return e.Reason + ": " + e.Err.Error() }
func (e *ConnectError) Unwrap() error { return e.Err }

// ConnectionMessage turns an OpenTarget error into a user-facing message.
func ConnectionMessage(err error) string {
	var ce *ConnectError
	if errors.As(err, &ce) {
		return "Tidak dapat terhubung ke database target: " + ce.Reason
	}
	return "Tidak dapat terhubung ke database target."
}

// Target is an open connection to a user-managed database.
type Target interface {
	Tables(ctx context.Context) ([]Table, error)
	Preview(ctx context.Context, table string, limit int) (StatementResult, error)
	Run(ctx context.Context, sql string, maxRows int) []StatementResult
	Close(ctx context.Context) error
}

type SchemaTarget interface {
	Schemas(ctx context.Context) ([]string, error)
	TablesInSchema(ctx context.Context, schema string) ([]Table, error)
	TableDetails(ctx context.Context, schema, table string) (TableDetails, error)
	PreviewPage(ctx context.Context, schema, table string, limit, offset int) (StatementResult, error)
}

type TransactionalTarget interface {
	RunInTx(ctx context.Context, statements []string, maxRows int) []StatementResult
}

type VersionedTarget interface {
	ApplyVersionedMigration(ctx context.Context, migration Migration, userID int64, maxRows int) (results []StatementResult, alreadyApplied bool, err error)
}

type Store interface {
	OpenTarget(ctx context.Context, dbID int64, readOnly bool) (Target, error)
	RecordMigration(ctx context.Context, record MigrationRecord) error
	RecentMigrations(ctx context.Context, dbID int64, limit int) ([]Migration, error)
}

type MigrationStore interface {
	CreateMigration(ctx context.Context, input MigrationInput) (Migration, error)
	UpdateMigration(ctx context.Context, input MigrationInput) (Migration, error)
	GetMigration(ctx context.Context, dbID, version int64) (Migration, error)
	RecordMigrationExecution(ctx context.Context, exec MigrationExecution) error
	RecentChanges(ctx context.Context, dbID int64, limit int) ([]ChangeLog, error)
}

type AttemptStore interface {
	BeginMigrationAttempt(ctx context.Context, migration Migration, userID int64) (int64, error)
	FinishMigrationAttempt(ctx context.Context, attemptID int64, exec MigrationExecution) error
}

type Access interface {
	ConnectionWithAccess(ctx context.Context, actor domain.User, dbID int64) (domain.Connection, authorization.AccessLevel, error)
}

type Service struct {
	access Access
	store  Store
}

func NewService(access Access, store Store) *Service {
	return &Service{access: access, store: store}
}

// View is everything database.html needs to render.
type View struct {
	Connection        domain.Connection
	Level             authorization.AccessLevel
	Unavailable       bool
	Schemas           []string
	Schema            string
	Tables            []Table
	Table             string
	Details           *TableDetails
	Page              int
	PrevPage          int
	NextPage          int
	HasPrev           bool
	HasNext           bool
	Preview           *StatementResult
	History           []Migration
	SelectedMigration *Migration
	MigrationTitle    string
	MigrationSQL      string
	PreviewOps        []string
	Changes           []ChangeLog
	SQL               string
	Results           []StatementResult
	Error             string
}

func (v View) CanEdit() bool { return v.Level.Allows(authorization.AccessViewEdit) }

func (s *Service) Browse(ctx context.Context, actor domain.User, dbID int64, table string) (View, error) {
	return s.BrowseTable(ctx, actor, dbID, "public", table, 1)
}

func (s *Service) BrowseTable(ctx context.Context, actor domain.User, dbID int64, schema, table string, page int) (View, error) {
	view, err := s.authorize(ctx, actor, dbID)
	if err != nil {
		return View{}, err
	}
	view.Schema = strings.TrimSpace(schema)
	if view.Schema == "" {
		view.Schema = "public"
	}
	if page < 1 {
		page = 1
	}
	view.Page = page
	view.PrevPage = page - 1
	view.NextPage = page + 1
	view.HasPrev = page > 1
	s.loadHistory(ctx, &view)
	s.loadChanges(ctx, &view)

	target, err := s.store.OpenTarget(ctx, dbID, true)
	if err != nil {
		log.Printf("open target database %d: %v", dbID, err)
		view.Error = ConnectionMessage(err)
		view.Unavailable = true
		return view, nil
	}
	defer target.Close(context.WithoutCancel(ctx))

	if inspector, ok := target.(SchemaTarget); ok {
		view.Schemas, err = inspector.Schemas(ctx)
		if err != nil {
			log.Printf("list schemas on database %d: %v", dbID, err)
			view.Error = "Gagal membaca daftar schema."
			return view, nil
		}
		view.Tables, err = inspector.TablesInSchema(ctx, view.Schema)
		if err != nil {
			log.Printf("list tables on database %d schema %q: %v", dbID, view.Schema, err)
			view.Error = "Gagal membaca daftar tabel."
			return view, nil
		}
	} else {
		s.loadTables(ctx, target, &view)
	}
	if table = strings.TrimSpace(table); table != "" && view.Error == "" {
		view.Table = table
		var preview StatementResult
		if inspector, ok := target.(SchemaTarget); ok {
			var details TableDetails
			details, err = inspector.TableDetails(ctx, view.Schema, table)
			if err == nil {
				view.Details = &details
				preview, err = inspector.PreviewPage(ctx, view.Schema, table, PreviewLimit, (page-1)*PreviewLimit)
			}
		} else {
			preview, err = target.Preview(ctx, table, PreviewLimit)
		}
		switch {
		case errors.Is(err, ErrTableNotFound):
			view.Error = "Tabel tidak ditemukan."
		case err != nil:
			log.Printf("preview table on database %d: %v", dbID, err)
			view.Error = "Gagal membaca isi tabel."
		default:
			view.Preview = &preview
			view.HasNext = preview.Truncated
		}
	}
	return view, nil
}

func (s *Service) ShowMigration(ctx context.Context, actor domain.User, dbID, version int64) (View, error) {
	view, err := s.Browse(ctx, actor, dbID, "")
	if err != nil {
		return View{}, err
	}
	ms, ok := s.store.(MigrationStore)
	if !ok {
		return view, admin.ErrNotFound
	}
	m, err := ms.GetMigration(ctx, dbID, version)
	if err != nil {
		return View{}, err
	}
	m.Operations = SplitStatements(m.SQL)
	view.SelectedMigration = &m
	view.PreviewOps = m.Operations
	if view.CanEdit() && m.Status != "SUCCESS" {
		view.MigrationTitle = m.Title
		view.MigrationSQL = m.SQL
	}
	return view, nil
}

func (s *Service) PreviewMigration(ctx context.Context, actor domain.User, dbID, version int64, title, sql string) (View, error) {
	view, err := s.authorize(ctx, actor, dbID)
	if err != nil {
		return View{}, err
	}
	view.MigrationTitle = strings.TrimSpace(title)
	view.MigrationSQL = sql
	if !view.CanEdit() {
		view.Error = "Akses VIEW_ONLY tidak dapat membuat atau mengubah migrasi."
		return s.reload(ctx, view, dbID), ErrReadOnly
	}
	if version > 0 {
		if ms, ok := s.store.(MigrationStore); ok {
			if m, err := ms.GetMigration(ctx, dbID, version); err == nil {
				m.Operations = SplitStatements(m.SQL)
				view.SelectedMigration = &m
			}
		}
	}
	ops := SplitStatements(sql)
	if len(ops) == 0 {
		view.Error = "SQL migrasi tidak boleh kosong."
		return s.reload(ctx, view, dbID), nil
	}
	if len(sql) > MaxSQLBytes {
		view.Error = "SQL migrasi terlalu panjang."
		return s.reload(ctx, view, dbID), nil
	}
	view.PreviewOps = ops
	return s.reload(ctx, view, dbID), nil
}

func (s *Service) CreateMigration(ctx context.Context, actor domain.User, dbID int64, title, sql string) (View, error) {
	view, err := s.authorize(ctx, actor, dbID)
	if err != nil {
		return View{}, err
	}
	view.MigrationTitle = strings.TrimSpace(title)
	view.MigrationSQL = sql
	if !view.CanEdit() {
		view.Error = "Akses VIEW_ONLY tidak dapat membuat migrasi."
		return s.reload(ctx, view, dbID), ErrReadOnly
	}
	ops := SplitStatements(sql)
	if len(ops) == 0 {
		view.Error = "SQL migrasi tidak boleh kosong."
		return s.reload(ctx, view, dbID), nil
	}
	if len(sql) > MaxSQLBytes {
		view.Error = "SQL migrasi terlalu panjang."
		return s.reload(ctx, view, dbID), nil
	}
	ms, ok := s.store.(MigrationStore)
	if !ok {
		return s.reload(ctx, view, dbID), errors.New("migration store not configured")
	}
	m, err := ms.CreateMigration(ctx, MigrationInput{
		DatabaseID: dbID,
		UserID:     actor.ID,
		Title:      view.MigrationTitle,
		SQL:        sql,
	})
	if err != nil {
		return View{}, err
	}
	m.Operations = ops
	view.SelectedMigration = &m
	view.PreviewOps = ops
	return s.reload(ctx, view, dbID), nil
}

// UpdateMigration modifies the title and SQL of a non-executed (DRAFT/FAILED) migration.
// Only VIEW_EDIT users may change migrations.
func (s *Service) UpdateMigration(ctx context.Context, actor domain.User, dbID, version int64, title, sql string) (View, error) {
	view, err := s.authorize(ctx, actor, dbID)
	if err != nil {
		return View{}, err
	}
	view.MigrationTitle = strings.TrimSpace(title)
	view.MigrationSQL = sql
	if !view.CanEdit() {
		view.Error = "Akses VIEW_ONLY tidak dapat mengubah migrasi."
		return s.reload(ctx, view, dbID), ErrReadOnly
	}
	ops := SplitStatements(sql)
	if len(ops) == 0 {
		view.Error = "SQL migrasi tidak boleh kosong."
		return s.reload(ctx, view, dbID), nil
	}
	if len(sql) > MaxSQLBytes {
		view.Error = "SQL migrasi terlalu panjang."
		return s.reload(ctx, view, dbID), nil
	}
	ms, ok := s.store.(MigrationStore)
	if !ok {
		return s.reload(ctx, view, dbID), errors.New("migration store not configured")
	}
	m, err := ms.UpdateMigration(ctx, MigrationInput{
		DatabaseID: dbID,
		UserID:     actor.ID,
		Version:    version,
		Title:      view.MigrationTitle,
		SQL:        sql,
	})
	if errors.Is(err, admin.ErrConflict) {
		view.Error = "Migrasi yang sudah berhasil dijalankan tidak dapat diubah."
		return s.reload(ctx, view, dbID), nil
	}
	if err != nil {
		return View{}, err
	}
	m.Operations = ops
	view.SelectedMigration = &m
	view.PreviewOps = ops
	return s.reload(ctx, view, dbID), nil
}

// ExecuteMigration runs a migration inside an explicit database transaction
// (COMMIT on success, ROLLBACK on failure) and records its status, execution
// duration, and database change log entry. Only VIEW_EDIT users may execute migrations.
func (s *Service) ExecuteMigration(ctx context.Context, actor domain.User, dbID, version int64) (View, error) {
	view, err := s.authorize(ctx, actor, dbID)
	if err != nil {
		return View{}, err
	}
	if !view.CanEdit() {
		view.Error = "Akses VIEW_ONLY tidak dapat menjalankan migrasi."
		return s.reload(ctx, view, dbID), ErrReadOnly
	}
	ms, ok := s.store.(MigrationStore)
	if !ok {
		return s.reload(ctx, view, dbID), errors.New("migration store not configured")
	}
	m, err := ms.GetMigration(ctx, dbID, version)
	if err != nil {
		return View{}, err
	}
	m.Operations = SplitStatements(m.SQL)
	view.SelectedMigration = &m
	view.PreviewOps = m.Operations
	if m.Status == "SUCCESS" {
		view.Error = "Migrasi ini sudah berhasil dijalankan."
		return s.reload(ctx, view, dbID), nil
	}
	if len(m.Operations) == 0 {
		view.Error = "SQL migrasi tidak boleh kosong."
		return s.reload(ctx, view, dbID), nil
	}
	attemptStore, tracksAttempts := s.store.(AttemptStore)
	var attemptID int64
	if tracksAttempts {
		attemptID, err = attemptStore.BeginMigrationAttempt(ctx, m, actor.ID)
		if errors.Is(err, admin.ErrConflict) {
			view.Error = "Migrasi sedang dijalankan, sudah selesai, atau draft berubah. Muat ulang riwayat."
			return s.reload(ctx, view, dbID), admin.ErrConflict
		}
		if err != nil {
			return View{}, err
		}
	}
	started := time.Now()
	finish := func(status, message string) error {
		exec := MigrationExecution{
			DatabaseID: dbID,
			Migration:  m,
			UserID:     actor.ID,
			Status:     status,
			Error:      message,
			DurationMs: time.Since(started).Milliseconds(),
		}
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if tracksAttempts {
			return attemptStore.FinishMigrationAttempt(auditCtx, attemptID, exec)
		}
		return ms.RecordMigrationExecution(auditCtx, exec)
	}

	target, err := s.store.OpenTarget(ctx, dbID, false)
	if err != nil {
		log.Printf("open target database %d: %v", dbID, err)
		view.Error = ConnectionMessage(err)
		view.Unavailable = true
		if auditErr := finish("FAILED", view.Error); auditErr != nil {
			log.Printf("record failed connection attempt on database %d: %v", dbID, auditErr)
		}
		s.loadHistory(ctx, &view)
		s.loadChanges(ctx, &view)
		return view, nil
	}
	defer target.Close(context.WithoutCancel(ctx))

	if versioned, ok := target.(VersionedTarget); ok {
		var already bool
		view.Results, already, err = versioned.ApplyVersionedMigration(ctx, m, actor.ID, MaxResultRows)
		if errors.Is(err, admin.ErrConflict) {
			view.Error = "Jalankan migrasi sebelumnya terlebih dahulu atau muat ulang riwayat."
			if auditErr := finish("FAILED", view.Error); auditErr != nil {
				log.Printf("record conflicted migration on database %d: %v", dbID, auditErr)
			}
			return s.reload(ctx, view, dbID), admin.ErrConflict
		}
		if err != nil {
			log.Printf("execute migration on database %d: %v", dbID, err)
			if auditErr := finish("FAILED", "Target migration execution failed"); auditErr != nil {
				log.Printf("record failed migration on database %d: %v", dbID, auditErr)
			}
			return View{}, err
		}
		if already {
			view.Results = []StatementResult{{Command: "Migrasi sudah ada di ledger target; mencocokkan status platform"}}
		}
	} else if txTarget, ok := target.(TransactionalTarget); ok {
		view.Results = txTarget.RunInTx(ctx, m.Operations, MaxResultRows)
	} else {
		view.Results = target.Run(ctx, m.SQL, MaxResultRows)
	}
	status, message := "SUCCESS", ""
	if msg := firstError(view.Results); msg != "" {
		status, message = "FAILED", msg
	}
	if err := finish(status, message); err != nil {
		log.Printf("record migration execution on database %d: %v", dbID, err)
		view.Error = "Status di target mungkin sudah berubah, tetapi pencatatan platform gagal. Muat ulang dan periksa riwayat sebelum mencoba lagi."
		return view, ErrAuditFailed
	}
	if updated, err := ms.GetMigration(ctx, dbID, version); err == nil {
		updated.Operations = m.Operations
		view.SelectedMigration = &updated
		if updated.Status != "SUCCESS" {
			view.MigrationTitle = updated.Title
			view.MigrationSQL = updated.SQL
		}
	}
	s.loadTables(ctx, target, &view)
	s.loadHistory(ctx, &view)
	s.loadChanges(ctx, &view)
	return view, nil
}

// Execute runs SQL against the target database. VIEW_ONLY users are limited to
// read-only statements; this is enforced both by statement inspection and by a
// read-only database session.
func (s *Service) Execute(ctx context.Context, actor domain.User, dbID int64, sql string) (View, error) {
	view, err := s.authorize(ctx, actor, dbID)
	if err != nil {
		return View{}, err
	}
	view.SQL = sql
	if strings.TrimSpace(sql) == "" {
		view.Error = "SQL tidak boleh kosong."
		return s.reload(ctx, view, dbID), nil
	}
	if len(sql) > MaxSQLBytes {
		view.Error = "SQL terlalu panjang."
		return s.reload(ctx, view, dbID), nil
	}
	readOnly := !view.CanEdit()
	mutation := !IsReadOnlyStatement(sql)
	if readOnly && mutation {
		view.Error = "Akses VIEW_ONLY hanya boleh menjalankan query baca (SELECT)."
		return s.reload(ctx, view, dbID), ErrReadOnly
	}

	target, err := s.store.OpenTarget(ctx, dbID, readOnly)
	if err != nil {
		log.Printf("open target database %d: %v", dbID, err)
		view.Error = ConnectionMessage(err)
		view.Unavailable = true
		s.loadHistory(ctx, &view)
		s.loadChanges(ctx, &view)
		return view, nil
	}
	defer target.Close(context.WithoutCancel(ctx))

	start := time.Now()
	view.Results = target.Run(ctx, sql, MaxResultRows)
	durationMs := time.Since(start).Milliseconds()
	if !readOnly && mutation {
		record := MigrationRecord{
			DatabaseID: dbID,
			UserID:     actor.ID,
			SQL:        sql,
			Status:     "SUCCESS",
			DurationMs: durationMs,
		}
		if msg := firstError(view.Results); msg != "" {
			record.Status, record.Error = "FAILED", msg
		}
		if err := s.store.RecordMigration(context.WithoutCancel(ctx), record); err != nil {
			log.Printf("record migration on database %d: %v", dbID, err)
		}
	}
	s.loadTables(ctx, target, &view)
	s.loadHistory(ctx, &view)
	s.loadChanges(ctx, &view)
	return view, nil
}

func (s *Service) authorize(ctx context.Context, actor domain.User, dbID int64) (View, error) {
	connection, level, err := s.access.ConnectionWithAccess(ctx, actor, dbID)
	if err != nil {
		return View{}, err
	}
	if !level.Allows(authorization.AccessViewOnly) {
		return View{}, admin.ErrForbidden
	}
	return View{Connection: connection, Level: level}, nil
}

// reload fills the table list, migration history, and change log for a view.
func (s *Service) reload(ctx context.Context, view View, dbID int64) View {
	target, err := s.store.OpenTarget(ctx, dbID, true)
	if err == nil {
		defer target.Close(context.WithoutCancel(ctx))
		msg := view.Error
		s.loadTables(ctx, target, &view)
		if msg != "" {
			view.Error = msg
		}
	}
	s.loadHistory(ctx, &view)
	s.loadChanges(ctx, &view)
	return view
}

func (s *Service) loadTables(ctx context.Context, target Target, view *View) {
	tables, err := target.Tables(ctx)
	if err != nil {
		log.Printf("list tables on database %d: %v", view.Connection.ID, err)
		if view.Error == "" {
			view.Error = "Gagal membaca daftar tabel."
		}
		return
	}
	view.Tables = tables
}

// loadHistory loads migration history for both VIEW_ONLY and VIEW_EDIT users.
func (s *Service) loadHistory(ctx context.Context, view *View) {
	history, err := s.store.RecentMigrations(ctx, view.Connection.ID, historyLimit)
	if err != nil {
		log.Printf("list migrations on database %d: %v", view.Connection.ID, err)
		return
	}
	for i := range history {
		history[i].Operations = SplitStatements(history[i].SQL)
	}
	view.History = history
}

func (s *Service) loadChanges(ctx context.Context, view *View) {
	ms, ok := s.store.(MigrationStore)
	if !ok {
		return
	}
	changes, err := ms.RecentChanges(ctx, view.Connection.ID, historyLimit)
	if err != nil {
		log.Printf("list database changes on database %d: %v", view.Connection.ID, err)
		return
	}
	view.Changes = changes
}

func firstError(results []StatementResult) string {
	for _, r := range results {
		if r.Error != "" {
			return r.Error
		}
	}
	return ""
}

// SplitStatements splits a SQL script into individual statements separated by
// top-level semicolons, ignoring empty or comment-only segments.
func SplitStatements(sql string) []string {
	var stmts []string
	rest := sql
	for {
		i := topLevelSemicolon(rest)
		if i < 0 {
			if strings.TrimSpace(stripLeadingComments(rest)) != "" {
				stmts = append(stmts, strings.TrimSpace(rest))
			}
			return stmts
		}
		part := rest[:i]
		if strings.TrimSpace(stripLeadingComments(part)) != "" {
			stmts = append(stmts, strings.TrimSpace(part))
		}
		rest = rest[i+1:]
	}
}

func HasTransactionControl(sql string) bool {
	for _, statement := range SplitStatements(sql) {
		fields := strings.Fields(stripLeadingComments(statement))
		if len(fields) == 0 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "BEGIN", "START", "COMMIT", "END", "ROLLBACK", "ABORT", "SAVEPOINT", "RELEASE", "PREPARE":
			return true
		}
	}
	return false
}

var readOnlyKeywords = map[string]bool{
	"select": true, "with": true, "show": true, "explain": true, "values": true, "table": true,
}

// IsReadOnlyStatement reports whether sql is a single statement starting with a
// read-only keyword. It is a first-line check only; read-only sessions on the
// target database are the actual enforcement (e.g. for data-modifying CTEs).
func IsReadOnlyStatement(sql string) bool {
	rest := stripLeadingComments(sql)
	end := 0
	for end < len(rest) && (isLetter(rest[end])) {
		end++
	}
	if !readOnlyKeywords[strings.ToLower(rest[:end])] {
		return false
	}
	// Reject multiple statements: anything after a top-level ';' other than whitespace/comments.
	if i := topLevelSemicolon(rest); i >= 0 && strings.TrimSpace(stripLeadingComments(rest[i+1:])) != "" {
		return false
	}
	return true
}

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func stripLeadingComments(s string) string {
	for {
		s = strings.TrimLeft(s, " \t\r\n\f(")
		switch {
		case strings.HasPrefix(s, "--"):
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = s[i+1:]
			} else {
				return ""
			}
		case strings.HasPrefix(s, "/*"):
			if i := strings.Index(s, "*/"); i >= 0 {
				s = s[i+2:]
			} else {
				return ""
			}
		default:
			return s
		}
	}
}

// topLevelSemicolon returns the index of the first ';' outside quotes,
// comments and dollar-quoted strings, or -1.
func topLevelSemicolon(s string) int {
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == ';':
			return i
		case s[i] == '\'' || s[i] == '"':
			q := s[i]
			for i++; i < len(s) && s[i] != q; i++ {
			}
		case strings.HasPrefix(s[i:], "--"):
			for ; i < len(s) && s[i] != '\n'; i++ {
			}
		case strings.HasPrefix(s[i:], "/*"):
			if j := strings.Index(s[i+2:], "*/"); j >= 0 {
				i += j + 3
			} else {
				return -1
			}
		case s[i] == '$':
			j := strings.IndexByte(s[i+1:], '$')
			if j < 0 {
				continue
			}
			tag := s[i : i+j+2]
			if strings.Trim(tag[1:len(tag)-1], "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ_0123456789") != "" {
				continue
			}
			if k := strings.Index(s[i+len(tag):], tag); k >= 0 {
				i += len(tag) + k + len(tag) - 1
			} else {
				return -1
			}
		}
	}
	return -1
}
