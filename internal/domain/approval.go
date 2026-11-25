package domain

import "time"

type ReviewStatus string

const (
	ReviewStatusPending  ReviewStatus = "PENDING"
	ReviewStatusApproved ReviewStatus = "APPROVED"
	ReviewStatusRejected ReviewStatus = "REJECTED"
	ReviewStatusStale    ReviewStatus = "STALE"
)

type ApprovalStatus string

const (
	ApprovalStatusPending  ApprovalStatus = "PENDING"
	ApprovalStatusApproved ApprovalStatus = "APPROVED"
	ApprovalStatusRejected ApprovalStatus = "REJECTED"
	ApprovalStatusStale    ApprovalStatus = "STALE"
)

type ReviewRequest struct {
	ID             int64        `json:"id"`
	RevisionID     int64        `json:"revision_id"`
	DefinitionHash string       `json:"definition_hash"`
	Status         ReviewStatus `json:"status"`
	RequestedBy    int64        `json:"requested_by"`
	RequestedAt    time.Time    `json:"requested_at"`
	ResolvedAt     *time.Time   `json:"resolved_at,omitempty"`
}

type ApprovalDecision struct {
	ID        int64          `json:"id"`
	ReviewID  int64          `json:"review_id"`
	GroupID   int64          `json:"group_id"`
	LeaderID  int64          `json:"leader_id"`
	Status    ApprovalStatus `json:"status"`
	Comment   string         `json:"comment,omitempty"`
	DecidedAt *time.Time     `json:"decided_at,omitempty"`
}
