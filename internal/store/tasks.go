package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
)

// TaskRow mirrors one row of the `tasks` table.
type TaskRow struct {
	HubTaskID    string
	ContextID    string
	UpstreamID   string
	State        a2a.TaskState
	CreatedAt    string
	UpdatedAt    string
	LastTaskJSON sql.NullString
}

// Task returns the cached last-known Task snapshot, or nil.
func (t TaskRow) Task() *a2a.Task {
	if !t.LastTaskJSON.Valid || t.LastTaskJSON.String == "" {
		return nil
	}
	var task a2a.Task
	if err := json.Unmarshal([]byte(t.LastTaskJSON.String), &task); err != nil {
		return nil
	}
	return &task
}

// CreateTask inserts a fresh task row (state = submitted) and returns nothing.
// The caller mints the hub_task_id.
func (s *Store) CreateTask(ctx context.Context, hubTaskID, contextID, upstreamID string) error {
	now := nowUTC()
	_, err := s.db.ExecContext(s.withCtx(ctx),
		`INSERT INTO tasks (hub_task_id, context_id, upstream_id, state, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		hubTaskID, contextID, upstreamID, string(a2a.TaskStateSubmitted), now, now,
	)
	if err != nil {
		return fmt.Errorf("create task %s: %w", hubTaskID, err)
	}
	return nil
}

// UpdateTaskSnapshot updates the state and cached JSON of a task.
func (s *Store) UpdateTaskSnapshot(ctx context.Context, hubTaskID string, state a2a.TaskState, task *a2a.Task) error {
	var snap sql.NullString
	if task != nil {
		b, err := json.Marshal(task)
		if err != nil {
			return fmt.Errorf("marshal task snapshot: %w", err)
		}
		snap = sql.NullString{String: string(b), Valid: true}
	}
	_, err := s.db.ExecContext(s.withCtx(ctx),
		`UPDATE tasks SET state = ?, updated_at = ?, last_task_json = ? WHERE hub_task_id = ?`,
		string(state), nowUTC(), snap, hubTaskID,
	)
	if err != nil {
		return fmt.Errorf("update task snapshot %s: %w", hubTaskID, err)
	}
	return nil
}

// GetTask returns a task row by hub_task_id, or ErrNotFound.
func (s *Store) GetTask(ctx context.Context, hubTaskID string) (TaskRow, error) {
	const q = `SELECT hub_task_id, context_id, upstream_id, state, created_at,
	                  updated_at, last_task_json
	           FROM tasks WHERE hub_task_id = ?`
	row := s.db.QueryRowContext(s.withCtx(ctx), q, hubTaskID)
	var t TaskRow
	err := row.Scan(&t.HubTaskID, &t.ContextID, &t.UpstreamID, &t.State,
		&t.CreatedAt, &t.UpdatedAt, &t.LastTaskJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskRow{}, ErrNotFound
	}
	if err != nil {
		return TaskRow{}, fmt.Errorf("get task %s: %w", hubTaskID, err)
	}
	return t, nil
}

// LookupContext returns the upstream_id of the most recent non-terminal task
// with the given contextId — this powers router "context stickiness."
func (s *Store) LookupContext(ctx context.Context, contextID string) (string, bool) {
	const q = `
		SELECT upstream_id FROM tasks
		WHERE context_id = ?
		  AND state IN ('submitted','working','input-required')
		ORDER BY updated_at DESC LIMIT 1
	`
	var upstreamID string
	err := s.db.QueryRowContext(s.withCtx(ctx), q, contextID).Scan(&upstreamID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		slog.Warn("LookupContext: unexpected DB error", "context_id", contextID, "err", err)
		return "", false
	}
	return upstreamID, true
}

// LookupHubTaskByContext returns the hub_task_id of the most recent non-terminal
// task for (contextID, upstreamID). Used to reuse the same hub-visible task id
// across follow-up turns of a multi-turn conversation.
func (s *Store) LookupHubTaskByContext(ctx context.Context, contextID, upstreamID string) (string, bool) {
	const q = `
		SELECT hub_task_id FROM tasks
		WHERE context_id = ? AND upstream_id = ?
		  AND state IN ('submitted','working','input-required')
		ORDER BY updated_at DESC LIMIT 1
	`
	var id string
	err := s.db.QueryRowContext(s.withCtx(ctx), q, contextID, upstreamID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	if err != nil {
		slog.Warn("LookupHubTaskByContext: unexpected DB error",
			"context_id", contextID, "upstream_id", upstreamID, "err", err)
		return "", false
	}
	return id, true
}

// CountActiveTasks returns the count of tasks in any non-terminal state
// (used for /metrics).
func (s *Store) CountActiveTasks(ctx context.Context) (int, error) {
	const q = `SELECT COUNT(*) FROM tasks WHERE state IN ('submitted','working','input-required')`
	var n int
	err := s.db.QueryRowContext(s.withCtx(ctx), q).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count active tasks: %w", err)
	}
	return n, nil
}

// --- task_id_map -----------------------------------------------------------

// MapTaskID records the upstream-issued task ID for a hub task.
func (s *Store) MapTaskID(ctx context.Context, hubTaskID, upstreamID, upstreamTaskID string) error {
	_, err := s.db.ExecContext(s.withCtx(ctx),
		`INSERT INTO task_id_map (hub_task_id, upstream_id, upstream_task_id)
		 VALUES (?, ?, ?)
		 ON CONFLICT(hub_task_id) DO UPDATE SET
		   upstream_id = excluded.upstream_id,
		   upstream_task_id = excluded.upstream_task_id`,
		hubTaskID, upstreamID, upstreamTaskID,
	)
	if err != nil {
		return fmt.Errorf("map task id %s -> %s: %w", hubTaskID, upstreamTaskID, err)
	}
	return nil
}

// LookupUpstreamTaskID returns the upstream_task_id for a hub_task_id, or ErrNotFound.
func (s *Store) LookupUpstreamTaskID(ctx context.Context, hubTaskID string) (string, error) {
	var id string
	err := s.db.QueryRowContext(s.withCtx(ctx),
		`SELECT upstream_task_id FROM task_id_map WHERE hub_task_id = ?`, hubTaskID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lookup upstream task id %s: %w", hubTaskID, err)
	}
	return id, nil
}

// --- List / detail / counts ------------------------------------------------

// TaskListFilter controls which tasks ListTasks returns.
type TaskListFilter struct {
	States     []a2a.TaskState
	UpstreamID string
	ContextID  string
	Recent     bool // when true and States is empty, include terminal states
	Limit      int
	Offset     int
}

// TaskListRow is a single row from ListTasks.
type TaskListRow struct {
	HubTaskID      string        `json:"hub_task_id"`
	ContextID      string        `json:"context_id"`
	UpstreamID     string        `json:"upstream_id"`
	UpstreamTaskID string        `json:"upstream_task_id"`
	State          a2a.TaskState `json:"state"`
	CreatedAt      string        `json:"created_at"`
	UpdatedAt      string        `json:"updated_at"`
	HasSnapshot    bool          `json:"has_snapshot"`
}

// TaskCounts aggregates task counts by state.
type TaskCounts struct {
	Submitted     int `json:"submitted"`
	Working       int `json:"working"`
	InputRequired int `json:"input_required"`
	Completed     int `json:"completed"`
	Failed        int `json:"failed"`
	Canceled      int `json:"canceled"`
	Total         int `json:"total"`
}

// TaskDetail combines a TaskRow with its mapped upstream task ID.
type TaskDetail struct {
	Task           TaskRow `json:"task"`
	UpstreamTaskID string  `json:"upstream_task_id"`
}

// ListTasks returns tasks matching the filter. If no States are provided and
// Recent is false, only non-terminal tasks are returned.
func (s *Store) ListTasks(ctx context.Context, f TaskListFilter) ([]TaskListRow, error) {
	query, args := buildTaskListQuery("SELECT", f)
	rows, err := s.db.QueryContext(s.withCtx(ctx), query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	var out []TaskListRow
	for rows.Next() {
		var r TaskListRow
		var upTaskID sql.NullString
		var hasSn int
		if err := rows.Scan(&r.HubTaskID, &r.ContextID, &r.UpstreamID,
			&upTaskID, &r.State, &r.CreatedAt, &r.UpdatedAt, &hasSn); err != nil {
			return nil, fmt.Errorf("scan task row: %w", err)
		}
		if upTaskID.Valid {
			r.UpstreamTaskID = upTaskID.String
		}
		r.HasSnapshot = hasSn != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountTasks returns the total number of tasks matching the filter (for pagination).
func (s *Store) CountTasks(ctx context.Context, f TaskListFilter) (int, error) {
	query, args := buildTaskListQuery("COUNT", f)
	var n int
	if err := s.db.QueryRowContext(s.withCtx(ctx), query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count tasks: %w", err)
	}
	return n, nil
}

// CountTasksByState returns aggregate counts for every task state.
func (s *Store) CountTasksByState(ctx context.Context) (TaskCounts, error) {
	const q = `SELECT state, COUNT(*) FROM tasks GROUP BY state`
	rows, err := s.db.QueryContext(s.withCtx(ctx), q)
	if err != nil {
		return TaskCounts{}, fmt.Errorf("count tasks by state: %w", err)
	}
	defer rows.Close()

	var c TaskCounts
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return TaskCounts{}, fmt.Errorf("scan task count: %w", err)
		}
		c.Total += n
		switch a2a.TaskState(state) {
		case a2a.TaskStateSubmitted:
			c.Submitted = n
		case a2a.TaskStateWorking:
			c.Working = n
		case a2a.TaskStateInputRequired:
			c.InputRequired = n
		case a2a.TaskStateCompleted:
			c.Completed = n
		case a2a.TaskStateFailed:
			c.Failed = n
		case a2a.TaskStateCanceled:
			c.Canceled = n
		}
	}
	return c, rows.Err()
}

// GetTaskDetail returns a TaskRow with its mapped upstream task ID.
func (s *Store) GetTaskDetail(ctx context.Context, hubTaskID string) (TaskDetail, error) {
	task, err := s.GetTask(ctx, hubTaskID)
	if err != nil {
		return TaskDetail{}, err
	}
	upID, _ := s.LookupUpstreamTaskID(ctx, hubTaskID)
	return TaskDetail{Task: task, UpstreamTaskID: upID}, nil
}

// buildTaskListQuery assembles the SELECT or COUNT query for tasks.
func buildTaskListQuery(mode string, f TaskListFilter) (string, []any) {
	var args []any
	var where []string

	// State filter.
	if len(f.States) > 0 {
		placeholders := make([]string, len(f.States))
		for i, s := range f.States {
			placeholders[i] = "?"
			args = append(args, string(s))
		}
		where = append(where, "t.state IN ("+join(placeholders, ",")+")")
	} else if !f.Recent {
		// Default: active (non-terminal) tasks only.
		where = append(where, "t.state IN ('submitted','working','input-required')")
	}
	if f.UpstreamID != "" {
		where = append(where, "t.upstream_id = ?")
		args = append(args, f.UpstreamID)
	}
	if f.ContextID != "" {
		where = append(where, "t.context_id = ?")
		args = append(args, f.ContextID)
	}

	whereClause := ""
	if len(where) > 0 {
		whereClause = " WHERE " + join(where, " AND ")
	}

	if mode == "COUNT" {
		return "SELECT COUNT(*) FROM tasks t" + whereClause, args
	}

	q := `SELECT t.hub_task_id, t.context_id, t.upstream_id,
	             m.upstream_task_id, t.state, t.created_at, t.updated_at,
	             CASE WHEN t.last_task_json IS NOT NULL AND t.last_task_json != '' THEN 1 ELSE 0 END
	      FROM tasks t
	      LEFT JOIN task_id_map m ON m.hub_task_id = t.hub_task_id` +
		whereClause +
		` ORDER BY t.updated_at DESC, t.created_at DESC`

	lim := f.Limit
	if lim <= 0 {
		lim = 50
	}
	q += fmt.Sprintf(" LIMIT %d", lim)
	if f.Offset > 0 {
		q += fmt.Sprintf(" OFFSET %d", f.Offset)
	}
	return q, args
}

// join is a minimal strings.Join to avoid importing "strings" for one call.
func join(elems []string, sep string) string {
	if len(elems) == 0 {
		return ""
	}
	n := len(sep) * (len(elems) - 1)
	for _, e := range elems {
		n += len(e)
	}
	b := make([]byte, 0, n)
	for i, e := range elems {
		if i > 0 {
			b = append(b, sep...)
		}
		b = append(b, e...)
	}
	return string(b)
}
