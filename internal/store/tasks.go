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
