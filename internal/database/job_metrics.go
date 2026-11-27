package database

import (
	"context"
	"database/sql"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

func JobStatusCounts(db *sql.DB) func(context.Context) (map[domain.JobStatus]int64, error) {
	return func(ctx context.Context) (map[domain.JobStatus]int64, error) {
		rows, err := db.QueryContext(ctx, `SELECT status, count(*) FROM migration_jobs GROUP BY status`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		counts := map[domain.JobStatus]int64{}
		for rows.Next() {
			var status string
			var n int64
			if err := rows.Scan(&status, &n); err != nil {
				return nil, err
			}
			counts[domain.JobStatus(status)] = n
		}
		return counts, rows.Err()
	}
}
