package migrationengine

import (
	"context"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

// RevisionStore menyediakan definisi migration immutable yang dibutuhkan engine.
type RevisionStore interface {
	// GetRevision mengambil satu revision pada database target. Implementasi harus
	// mengembalikan revision persis seperti yang tersimpan tanpa mengubah SQL-nya.
	GetRevision(ctx context.Context, databaseID, revisionID int64) (domain.MigrationRevision, error)

	// ListRevisions mengambil seluruh revision milik satu database untuk daftar
	// migration dan pemeriksaan urutan versi.
	ListRevisions(ctx context.Context, databaseID int64) ([]domain.MigrationRevision, error)
}

// HistoryStore menyimpan riwayat setiap percobaan eksekusi, termasuk kegagalan.
type HistoryStore interface {
	// AppendAttempt menambahkan satu catatan attempt baru. Implementasi tidak boleh
	// menimpa atau menghapus attempt lama karena history bersifat append-only.
	AppendAttempt(ctx context.Context, attempt domain.ExecutionAttempt) error

	// ListAttempts mengambil riwayat attempt pada satu database untuk ditampilkan
	// dan diproses oleh aturan Hide Rollback.
	ListAttempts(ctx context.Context, databaseID int64) ([]domain.ExecutionAttempt, error)
}

// TargetExecutor adalah batas antara engine dan PostgreSQL target.
type TargetExecutor interface {
	// Execute menjalankan revision yang telah dipilih sesuai action pada request.
	// Implementasi menangani transaction, ledger, checkpoint, dan idempotency,
	// lalu mengembalikan hasil tanpa mengurus HTTP, approval, atau queue.
	Execute(ctx context.Context, request domain.ExecutionRequest, revision domain.MigrationRevision) (domain.ExecutionResult, error)
}
