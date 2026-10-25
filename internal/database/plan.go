package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Minaqu28/IF3110_K03-06/internal/explorer"
)

var _ explorer.PlanTarget = (*pgTarget)(nil)

type postgresPlanDocument struct {
	Plan          explorer.PlanNode `json:"Plan"`
	PlanningTime  float64           `json:"Planning Time"`
	ExecutionTime float64           `json:"Execution Time"`
}

// Explain returns PostgreSQL's structured plan for a single read query. The
// pgTarget is opened with default_transaction_read_only=on by the service.
func (t *pgTarget) Explain(ctx context.Context, query string, analyze bool) (explorer.ExecutionPlan, error) {
	if !t.readOnly {
		return explorer.ExecutionPlan{}, errors.New("execution plan requires a read-only target")
	}
	options := "FORMAT JSON, VERBOSE TRUE, COSTS TRUE"
	if analyze {
		options += ", ANALYZE TRUE, BUFFERS TRUE, TIMING TRUE"
	}
	statement := fmt.Sprintf("EXPLAIN (%s) %s", options, query)
	var raw []byte
	if err := t.conn.QueryRow(ctx, statement).Scan(&raw); err != nil {
		return explorer.ExecutionPlan{}, &explorer.PlanQueryError{Message: describeError(err), Err: err}
	}
	var documents []postgresPlanDocument
	if err := json.Unmarshal(raw, &documents); err != nil {
		return explorer.ExecutionPlan{}, fmt.Errorf("decode execution plan: %w", err)
	}
	if len(documents) != 1 || documents[0].Plan.NodeType == "" {
		return explorer.ExecutionPlan{}, errors.New("PostgreSQL returned an empty execution plan")
	}
	return explorer.ExecutionPlan{
		PlanningTimeMs:  documents[0].PlanningTime,
		ExecutionTimeMs: documents[0].ExecutionTime,
		Root:            documents[0].Plan,
	}, nil
}
