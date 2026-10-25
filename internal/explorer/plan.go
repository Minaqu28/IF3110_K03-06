package explorer

import (
	"context"
	"errors"
	"log"
	"strings"

	"github.com/Minaqu28/IF3110_K03-06/internal/domain"
)

const (
	PlanModeEstimate = "estimate"
	PlanModeAnalyze  = "analyze"
)

// ErrInvalidPlanQuery identifies an invalid mode, a non-query statement, or a
// PostgreSQL planning error that is safe to return as a 400 response.
var ErrInvalidPlanQuery = errors.New("invalid execution plan query")

// PlanQueryError carries a PostgreSQL error message that is safe to show on the
// database page without exposing connection credentials.
type PlanQueryError struct {
	Message string
	Err     error
}

func (e *PlanQueryError) Error() string { return e.Message }
func (e *PlanQueryError) Unwrap() error { return e.Err }

// PlanNode represents one node in PostgreSQL's FORMAT JSON plan tree.
type PlanNode struct {
	NodeType            string     `json:"Node Type"`
	RelationName        string     `json:"Relation Name"`
	Alias               string     `json:"Alias"`
	IndexName           string     `json:"Index Name"`
	JoinType            string     `json:"Join Type"`
	StartupCost         float64    `json:"Startup Cost"`
	TotalCost           float64    `json:"Total Cost"`
	PlanRows            int64      `json:"Plan Rows"`
	PlanWidth           int64      `json:"Plan Width"`
	ActualStartupTime   *float64   `json:"Actual Startup Time"`
	ActualTotalTime     *float64   `json:"Actual Total Time"`
	ActualRows          *float64   `json:"Actual Rows"`
	ActualLoops         *float64   `json:"Actual Loops"`
	RowsRemovedByFilter int64      `json:"Rows Removed by Filter"`
	SharedHitBlocks     int64      `json:"Shared Hit Blocks"`
	SharedReadBlocks    int64      `json:"Shared Read Blocks"`
	Filter              string     `json:"Filter"`
	IndexCond           string     `json:"Index Cond"`
	HashCond            string     `json:"Hash Cond"`
	JoinFilter          string     `json:"Join Filter"`
	Plans               []PlanNode `json:"Plans"`
}

// ExecutionPlan is the structured result shown by the execution plan analyzer.
type ExecutionPlan struct {
	Query           string
	Analyze         bool
	PlanningTimeMs  float64
	ExecutionTimeMs float64
	NodeCount       int
	Root            PlanNode
}

type PlanTarget interface {
	Explain(ctx context.Context, query string, analyze bool) (ExecutionPlan, error)
}

// Explain validates and plans one read query. Both access levels may use it;
// the target is always opened read-only, including for EXPLAIN ANALYZE.
func (s *Service) Explain(ctx context.Context, actor domain.User, dbID int64, query, mode string) (View, error) {
	view, err := s.authorize(ctx, actor, dbID)
	if err != nil {
		return View{}, err
	}
	view.PlanSQL = query
	view.PlanMode = mode

	query = strings.TrimSpace(query)
	if query == "" {
		view.Error = "Query untuk execution plan tidak boleh kosong."
		return s.reload(ctx, view, dbID), ErrInvalidPlanQuery
	}
	if len(query) > MaxSQLBytes {
		view.Error = "Query untuk execution plan terlalu panjang."
		return s.reload(ctx, view, dbID), ErrInvalidPlanQuery
	}
	if !IsExplainableQuery(query) {
		view.Error = "Execution Plan hanya menerima satu query SELECT atau WITH ... SELECT."
		return s.reload(ctx, view, dbID), ErrInvalidPlanQuery
	}

	analyze := false
	switch mode {
	case PlanModeEstimate:
	case PlanModeAnalyze:
		analyze = true
	default:
		view.Error = "Mode execution plan tidak valid."
		return s.reload(ctx, view, dbID), ErrInvalidPlanQuery
	}

	target, err := s.store.OpenTarget(ctx, dbID, true)
	if err != nil {
		log.Printf("open target database %d for execution plan: %v", dbID, err)
		view.Error = ConnectionMessage(err)
		view.Unavailable = true
		s.loadHistory(ctx, &view)
		s.loadChanges(ctx, &view)
		return view, nil
	}
	defer target.Close(context.WithoutCancel(ctx))

	planner, ok := target.(PlanTarget)
	if !ok {
		return View{}, errors.New("execution plan is not supported by target")
	}
	plan, err := planner.Explain(ctx, query, analyze)
	if err != nil {
		var queryErr *PlanQueryError
		if errors.As(err, &queryErr) {
			view.Error = queryErr.Message
			return s.reload(ctx, view, dbID), ErrInvalidPlanQuery
		}
		return View{}, err
	}
	plan.Query = query
	plan.Analyze = analyze
	plan.NodeCount = countPlanNodes(plan.Root)
	view.Plan = &plan
	s.loadTables(ctx, target, &view)
	s.loadHistory(ctx, &view)
	s.loadChanges(ctx, &view)
	return view, nil
}

// IsExplainableQuery accepts one SELECT or WITH query. The target read-only
// session remains the final protection for data-modifying CTEs and functions.
func IsExplainableQuery(query string) bool {
	statements := SplitStatements(query)
	if len(statements) != 1 {
		return false
	}
	rest := stripLeadingComments(statements[0])
	end := 0
	for end < len(rest) && isLetter(rest[end]) {
		end++
	}
	switch strings.ToLower(rest[:end]) {
	case "select", "with":
		return true
	default:
		return false
	}
}

func countPlanNodes(root PlanNode) int {
	count := 0
	var walk func(PlanNode)
	walk = func(node PlanNode) {
		count++
		for _, child := range node.Plans {
			walk(child)
		}
	}
	walk(root)
	return count
}
