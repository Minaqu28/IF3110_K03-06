package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"strconv"
	"time"
)

type MigrationAction string

const (
	MigrationActionApply    MigrationAction = "APPLY"
	MigrationActionRollback MigrationAction = "ROLLBACK"
	MigrationActionForward  MigrationAction = "FORWARD"
)

type ExecutionMode string

const (
	ExecutionModeNormal ExecutionMode = "NORMAL"
	ExecutionModeBatch  ExecutionMode = "BATCH"
)

type RevisionStatus string

const (
	RevisionStatusDraft    RevisionStatus = "DRAFT"
	RevisionStatusInReview RevisionStatus = "IN_REVIEW"
	RevisionStatusApproved RevisionStatus = "APPROVED"
	RevisionStatusRejected RevisionStatus = "REJECTED"
)

type BatchConfig struct {
	Table        string `json:"table"`
	CursorColumn string `json:"cursor_column"`
	BatchSize    int    `json:"batch_size"`
}

type MigrationRevision struct {
	ID                 int64          `json:"id"`
	MigrationID        int64          `json:"migration_id"`
	DatabaseID         int64          `json:"database_id"`
	Revision           int            `json:"revision"`
	Title              string         `json:"title"`
	UpSQL              string         `json:"up_sql"`
	DownSQL            string         `json:"down_sql,omitempty"`
	Reversible         bool           `json:"reversible"`
	IrreversibleReason string         `json:"irreversible_reason,omitempty"`
	Status             RevisionStatus `json:"status"`
	DefinitionHash     string         `json:"definition_hash"`
	CreatedBy          int64          `json:"created_by"`
	CreatedAt          time.Time      `json:"created_at"`
	LockedAt           *time.Time     `json:"locked_at,omitempty"`
	Batch              *BatchConfig   `json:"batch,omitempty"`
}

type Direction string

const (
	DirectionUp   Direction = "UP"
	DirectionDown Direction = "DOWN"
)

func (a MigrationAction) Valid() bool {
	switch a {
	case MigrationActionApply, MigrationActionRollback, MigrationActionForward:
		return true
	}
	return false
}

func (a MigrationAction) Direction() Direction {
	if a == MigrationActionRollback {
		return DirectionDown
	}
	return DirectionUp
}

func (a MigrationAction) RequiresTargetExecution() bool {
	return a == MigrationActionRollback
}

const definitionHashVersion = "dbmp-revision-v1"

func ComputeDefinitionHash(rev MigrationRevision) string {
	h := sha256.New()
	writeField(h, definitionHashVersion)
	writeField(h, rev.UpSQL)
	writeField(h, rev.DownSQL)
	writeField(h, strconv.FormatBool(rev.Reversible))
	if rev.Batch == nil {
		writeField(h, string(ExecutionModeNormal))
	} else {
		writeField(h, string(ExecutionModeBatch))
		writeField(h, rev.Batch.Table)
		writeField(h, rev.Batch.CursorColumn)
		writeField(h, strconv.Itoa(rev.Batch.BatchSize))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeField(h hash.Hash, s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	h.Write(n[:])
	h.Write([]byte(s))
}
