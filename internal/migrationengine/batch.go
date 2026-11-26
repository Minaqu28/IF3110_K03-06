package migrationengine

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Minaqu28/IF3110_K03-06/internal/admin"
	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
)

const (
	MinBatchSize = 1
	MaxBatchSize = 100000
)

const ErrUnsafeRevertMessage = "pembatalan tidak aman; buat migration perbaikan baru"

var ErrInvalidBatch = fmt.Errorf("%w: invalid batch migration", admin.ErrInvalidInput)

var placeholder = regexp.MustCompile(`\$([0-9]+)`)

func ValidateBatch(rev domain.MigrationRevision) error {
	b := rev.Batch
	if b == nil {
		return fmt.Errorf("%w: the revision has no batch configuration", ErrInvalidBatch)
	}
	if err := validateTableName(b.Table); err != nil {
		return err
	}
	if strings.TrimSpace(b.CursorColumn) == "" || strings.Contains(b.CursorColumn, ".") {
		return fmt.Errorf("%w: the cursor column must be a single column name", ErrInvalidBatch)
	}
	if b.BatchSize < MinBatchSize || b.BatchSize > MaxBatchSize {
		return fmt.Errorf("%w: batch size must be between %d and %d", ErrInvalidBatch, MinBatchSize, MaxBatchSize)
	}
	if err := ValidateBatchSQL("up", rev.UpSQL); err != nil {
		return err
	}
	if rev.Reversible {
		return ValidateBatchSQL("down", rev.DownSQL)
	}
	return nil
}

func validateTableName(table string) error {
	parts := strings.Split(table, ".")
	if len(parts) > 2 {
		return fmt.Errorf("%w: the table must be \"table\" or \"schema.table\"", ErrInvalidBatch)
	}
	for _, p := range parts {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("%w: the table must be \"table\" or \"schema.table\"", ErrInvalidBatch)
		}
	}
	return nil
}

func ValidateBatchSQL(direction, sql string) error {
	if statements := explorer.SplitStatements(sql); len(statements) != 1 {
		return fmt.Errorf("%w: batch %s SQL must be exactly one statement, found %d", ErrInvalidBatch, direction, len(statements))
	}
	if explorer.HasTransactionControl(sql) {
		return fmt.Errorf("%w: batch %s SQL must not contain transaction control", ErrInvalidBatch, direction)
	}
	seen := map[string]bool{}
	for _, m := range placeholder.FindAllStringSubmatch(sql, -1) {
		seen[m[1]] = true
	}
	if !seen["1"] || !seen["2"] {
		return fmt.Errorf("%w: batch %s SQL must use $1 (exclusive lower bound) and $2 (inclusive upper bound)", ErrInvalidBatch, direction)
	}
	for p := range seen {
		if p != "1" && p != "2" {
			return fmt.Errorf("%w: batch %s SQL may only use $1 and $2, found $%s", ErrInvalidBatch, direction, p)
		}
	}
	return nil
}
