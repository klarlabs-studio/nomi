package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"go.klarlabs.de/nomi/internal/domain"
)

// marshalArguments encodes a step's argument map for the JSON column. Empty
// maps become NULL so the column is sparse and old plans (no arguments)
// keep round-tripping through the same code path.
func marshalArguments(args map[string]interface{}) (interface{}, error) {
	if len(args) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal step arguments: %w", err)
	}
	return string(b), nil
}

func unmarshalArguments(raw sql.NullString) (map[string]interface{}, error) {
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(raw.String), &args); err != nil {
		return nil, fmt.Errorf("failed to unmarshal step arguments: %w", err)
	}
	return args, nil
}

// PlanRepository handles plan persistence
type PlanRepository struct {
	db *DB
}

// NewPlanRepository creates a new repository
func NewPlanRepository(db *DB) *PlanRepository {
	return &PlanRepository{db: db}
}

// Create inserts a new plan with its step definitions
func (r *PlanRepository) Create(plan *domain.Plan) error {
	tx, err := r.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Insert plan
	_, err = tx.Exec(
		`INSERT INTO plans (id, run_id, version, created_at) VALUES (?, ?, ?, ?)`,
		plan.ID, plan.RunID, plan.Version, plan.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to create plan: %w", err)
	}

	// Insert step definitions
	for _, step := range plan.Steps {
		argsCol, err := marshalArguments(step.Arguments)
		if err != nil {
			return err
		}
		_, err = tx.Exec(
			`INSERT INTO step_definitions (id, plan_id, title, description, expected_tool, expected_capability, arguments, step_order, created_at, why)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			step.ID, step.PlanID, step.Title, step.Description, step.ExpectedTool, step.ExpectedCapability, argsCol, step.Order, step.CreatedAt, step.Why,
		)
		if err != nil {
			return fmt.Errorf("failed to create step definition: %w", err)
		}
		for _, depID := range step.DependsOn {
			_, err = tx.Exec(
				`INSERT INTO step_dependencies (step_id, depends_on_step_id) VALUES (?, ?)`,
				step.ID, depID,
			)
			if err != nil {
				return fmt.Errorf("failed to create step dependency: %w", err)
			}
		}
	}

	return tx.Commit()
}

// GetByRunID retrieves the latest plan for a run
func (r *PlanRepository) GetByRunID(runID string) (*domain.Plan, error) {
	var plan domain.Plan
	err := r.db.QueryRow(
		`SELECT id, run_id, version, created_at FROM plans WHERE run_id = ? ORDER BY version DESC, created_at DESC LIMIT 1`,
		runID,
	).Scan(&plan.ID, &plan.RunID, &plan.Version, &plan.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get plan: %w", err)
	}

	// Load step definitions
	steps, err := r.GetStepDefinitions(plan.ID)
	if err != nil {
		return nil, err
	}
	plan.Steps = steps

	return &plan, nil
}

// GetStepDefinition retrieves a single step definition by ID. Used by the
// runtime's determineTool to route a step to its declared ExpectedTool.
func (r *PlanRepository) GetStepDefinition(id string) (*domain.StepDefinition, error) {
	var step domain.StepDefinition
	var argsRaw sql.NullString
	var whyRaw sql.NullString
	err := r.db.QueryRow(
		`SELECT id, plan_id, title, description, expected_tool, expected_capability, arguments, step_order, created_at, why
		 FROM step_definitions WHERE id = ?`,
		id,
	).Scan(&step.ID, &step.PlanID, &step.Title, &step.Description,
		&step.ExpectedTool, &step.ExpectedCapability, &argsRaw, &step.Order, &step.CreatedAt, &whyRaw)
	if whyRaw.Valid {
		step.Why = whyRaw.String
	}
	if err != nil {
		return nil, err
	}
	step.Arguments, err = unmarshalArguments(argsRaw)
	if err != nil {
		return nil, err
	}
	step.DependsOn = r.loadDependencies(id)
	return &step, nil
}

// GetStepDefinitions retrieves all step definitions for a plan. Steps and
// their step_dependencies rows load in two queries total — never one query
// per step inside a `rows.Next()` loop, which historically deadlocked the
// SQLite connection pool when nested-query callbacks waited for a free
// connection that the outer iteration was holding.
func (r *PlanRepository) GetStepDefinitions(planID string) ([]domain.StepDefinition, error) {
	rows, err := r.db.Query(
		`SELECT id, plan_id, title, description, expected_tool, expected_capability, arguments, step_order, created_at, why
		 FROM step_definitions WHERE plan_id = ? ORDER BY step_order`,
		planID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get step definitions: %w", err)
	}
	defer rows.Close()

	steps := make([]domain.StepDefinition, 0)
	stepIDs := make([]string, 0)
	for rows.Next() {
		var step domain.StepDefinition
		var argsRaw sql.NullString
		var whyRaw sql.NullString
		if err := rows.Scan(&step.ID, &step.PlanID, &step.Title, &step.Description,
			&step.ExpectedTool, &step.ExpectedCapability, &argsRaw, &step.Order, &step.CreatedAt, &whyRaw); err != nil {
			return nil, fmt.Errorf("failed to scan step definition: %w", err)
		}
		if whyRaw.Valid {
			step.Why = whyRaw.String
		}
		args, err := unmarshalArguments(argsRaw)
		if err != nil {
			return nil, fmt.Errorf("step %s: %w", step.ID, err)
		}
		step.Arguments = args
		steps = append(steps, step)
		stepIDs = append(stepIDs, step.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	depsByStep, err := r.loadDependenciesBatch(stepIDs)
	if err != nil {
		return nil, err
	}
	for i := range steps {
		steps[i].DependsOn = depsByStep[steps[i].ID]
	}
	return steps, nil
}

// loadDependenciesBatch fetches every step_dependencies row for the given
// step IDs in a single IN(...) query and groups the results by parent. The
// per-step nested-query pattern this replaces could deadlock when the
// connection pool was sized to 1; even with a larger pool, batching is one
// round trip per plan instead of N.
func (r *PlanRepository) loadDependenciesBatch(stepIDs []string) (map[string][]string, error) {
	out := make(map[string][]string, len(stepIDs))
	if len(stepIDs) == 0 {
		return out, nil
	}
	placeholders := strings.Repeat("?,", len(stepIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]interface{}, len(stepIDs))
	for i, id := range stepIDs {
		args[i] = id
	}
	query := fmt.Sprintf(
		`SELECT step_id, depends_on_step_id FROM step_dependencies WHERE step_id IN (%s)`,
		placeholders,
	)
	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to load step dependencies: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var stepID, depID string
		if err := rows.Scan(&stepID, &depID); err != nil {
			return nil, fmt.Errorf("failed to scan step dependency: %w", err)
		}
		out[stepID] = append(out[stepID], depID)
	}
	return out, rows.Err()
}

// loadDependencies retains the single-step API used by GetStepDefinition.
// It runs one query per call, which is fine when the caller is loading a
// single row (no outer iteration, no pool starvation).
func (r *PlanRepository) loadDependencies(stepID string) []string {
	deps, err := r.loadDependenciesBatch([]string{stepID})
	if err != nil {
		return nil
	}
	return deps[stepID]
}

// ListByRunID retrieves all plans for a run
func (r *PlanRepository) ListByRunID(runID string) ([]domain.Plan, error) {
	rows, err := r.db.Query(
		`SELECT id, run_id, version, created_at FROM plans WHERE run_id = ? ORDER BY version DESC`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list plans: %w", err)
	}
	defer rows.Close()

	plans := make([]domain.Plan, 0)
	for rows.Next() {
		var plan domain.Plan
		err := rows.Scan(&plan.ID, &plan.RunID, &plan.Version, &plan.CreatedAt)
		if err != nil {
			continue
		}
		steps, err := r.GetStepDefinitions(plan.ID)
		if err != nil {
			continue
		}
		plan.Steps = steps
		plans = append(plans, plan)
	}
	return plans, rows.Err()
}

// GetPlanVersion retrieves the next plan version for a run
func (r *PlanRepository) GetPlanVersion(runID string) (int, error) {
	var version int
	err := r.db.QueryRow(
		`SELECT COALESCE(MAX(version), 0) + 1 FROM plans WHERE run_id = ?`,
		runID,
	).Scan(&version)
	if err != nil {
		return 1, nil
	}
	return version, nil
}
