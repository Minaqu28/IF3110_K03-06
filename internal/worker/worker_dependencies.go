package worker

import (
	"context"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

// ExecutionAuthorizer memvalidasi ulang izin, approval, dan kondisi target tepat sebelum worker memanggil migration engine.
type ExecutionAuthorizer interface {
	// AuthorizeExecution mengembalikan nil hanya jika request masih boleh dijalankan. 
	// Implementasi harus membaca state terbaru, bukan mempercayai flag approval atau permission dari payload queue.
	AuthorizeExecution(ctx context.Context, request domain.ExecutionRequest) error
}

// MigrationExecutor menghubungkan worker dengan engine milik Anggota 2.
type MigrationExecutor interface {
	// Execute meneruskan satu request yang sudah direvalidasi ke engine. Retry job
	// yang sama harus tetap memakai execution ID yang sama agar idempotent.
	Execute(ctx context.Context, request domain.ExecutionRequest) (domain.ExecutionResult, error)
}

// JobStateStore menyimpan state job secara persisten dan terpisah dari broker.
type JobStateStore interface {
	// GetJob mengambil state job terbaru sebelum worker memproses atau melanjutkan
	// pesan yang dikirim ulang.
	GetJob(ctx context.Context, jobID string) (domain.Job, error)

	// SaveJob menyimpan perubahan status, attempt, error, atau progress sebelum
	// worker melakukan acknowledgement terhadap pesan.
	SaveJob(ctx context.Context, job domain.Job) error
}

// JobDelivery membungkus satu pesan queue tanpa mengikat worker pada library
// broker tertentu.
type JobDelivery interface {
	// Message mengembalikan payload job yang sudah berhasil didecode.
	Message() domain.MigrationJobMessage

	// Redelivered bernilai true jika broker pernah mengirim pesan ini sebelumnya.
	Redelivered() bool

	// Ack menandai pesan selesai dan hanya dipanggil setelah hasil job tersimpan.
	Ack() error

	// Requeue mengembalikan pesan untuk retry ketika error masih dapat dipulihkan.
	Requeue() error

	// Reject menolak pesan tanpa retry, misalnya untuk payload yang tidak valid.
	Reject() error
}

// JobConsumer menyediakan pesan job kepada worker.
type JobConsumer interface {
	// Receive menunggu satu delivery atau berhenti ketika context dibatalkan.
	Receive(ctx context.Context) (JobDelivery, error)
}
