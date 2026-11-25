package workflow

import (
	"context"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

// ApprovalStore menyimpan review request dan keputusan setiap leader wajib.
type ApprovalStore interface {
	// CreateReview menyimpan review baru yang terikat pada revision tertentu dan
	// mengembalikan data lengkapnya, termasuk ID yang dibuat storage.
	CreateReview(ctx context.Context, review domain.ReviewRequest) (domain.ReviewRequest, error)

	// ListDecisions mengambil keputusan seluruh leader untuk satu review agar
	// service dapat menentukan apakah approval sudah lengkap.
	ListDecisions(ctx context.Context, reviewID int64) ([]domain.ApprovalDecision, error)

	// SaveDecision menyimpan keputusan satu leader. Pemeriksaan apakah actor
	// memang leader yang berhak tetap dilakukan oleh service workflow.
	SaveDecision(ctx context.Context, decision domain.ApprovalDecision) (domain.ApprovalDecision, error)
}

// JobStore adalah sumber status job persisten untuk workflow dan polling UI.
type JobStore interface {
	// CreateJob menyimpan job sebelum pesan dipublish agar kegagalan publish dapat
	// dipulihkan dan status job tetap dapat diaudit.
	CreateJob(ctx context.Context, job domain.Job) error

	// GetJob mengambil status job terbaru berdasarkan job ID untuk polling dan
	// proses recovery.
	GetJob(ctx context.Context, jobID string) (domain.Job, error)
}

// JobPublisher adalah batas minimal workflow menuju message queue.
type JobPublisher interface {
	// Publish mengirim referensi job yang sudah tersimpan. Implementasi nantinya
	// tidak boleh memasukkan SQL, password, cookie, atau secret ke pesan.
	Publish(ctx context.Context, message domain.MigrationJobMessage) error
}
