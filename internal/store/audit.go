package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// AuditEvent enumerates the dispatch events written to audit_log.
type AuditEvent string

const (
	EventSend           AuditEvent = "send"
	EventForward        AuditEvent = "forward"
	EventResponse       AuditEvent = "resp"
	EventError          AuditEvent = "error"
	EventCancel         AuditEvent = "cancel"
	EventBreakerOpen    AuditEvent = "breaker-open"
	EventBreakerBlocked AuditEvent = "breaker-blocked"
	EventBreakerClose   AuditEvent = "breaker-close"
	EventStreamStart    AuditEvent = "stream-start"
	EventStreamEnd      AuditEvent = "stream-end"
	EventCardRefresh    AuditEvent = "card-refresh"
)

// AuditEntry is a row to write to audit_log.
type AuditEntry struct {
	TraceID    string
	HubTaskID  string
	UpstreamID string
	Event      AuditEvent
	Detail     any
}

// WriteAudit appends an event to audit_log. Failures are returned but callers
// often ignore them (audit logging must not block dispatch).
func (s *Store) WriteAudit(ctx context.Context, e AuditEntry) error {
	var detail string
	if e.Detail != nil {
		b, err := json.Marshal(e.Detail)
		if err != nil {
			return fmt.Errorf("marshal audit detail: %w", err)
		}
		detail = string(b)
	}
	_, err := s.db.ExecContext(s.withCtx(ctx),
		`INSERT INTO audit_log (ts, trace_id, hub_task_id, upstream_id, event, detail_json)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		nowUTC(), e.TraceID, e.HubTaskID, e.UpstreamID, string(e.Event), detail,
	)
	if err != nil {
		return fmt.Errorf("write audit: %w", err)
	}
	return nil
}

// VacuumAudit trims audit_log to the newest max rows. Called on startup.
func (s *Store) VacuumAudit(ctx context.Context, max int) error {
	if max <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(s.withCtx(ctx),
		`DELETE FROM audit_log WHERE id NOT IN (
		    SELECT id FROM audit_log ORDER BY id DESC LIMIT ?
		)`, max)
	if err != nil {
		return fmt.Errorf("vacuum audit: %w", err)
	}
	return nil
}

// --- List / count -----------------------------------------------------------

// AuditListFilter controls which audit entries ListAudit returns.
type AuditListFilter struct {
	UpstreamID string
	HubTaskID  string
	TraceID    string
	Event      AuditEvent
	Limit      int
	Offset     int
}

// AuditListRow is a single row from ListAudit.
type AuditListRow struct {
	ID         int64      `json:"id"`
	TS         string     `json:"ts"`
	TraceID    string     `json:"trace_id"`
	HubTaskID  string     `json:"hub_task_id"`
	UpstreamID string     `json:"upstream_id"`
	Event      AuditEvent `json:"event"`
	DetailJSON string     `json:"detail_json,omitempty"`
}

// ListAudit returns audit log entries matching the filter, newest first.
func (s *Store) ListAudit(ctx context.Context, f AuditListFilter) ([]AuditListRow, error) {
	query, args := buildAuditListQuery("SELECT", f)
	rows, err := s.db.QueryContext(s.withCtx(ctx), query, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	defer rows.Close()

	var out []AuditListRow
	for rows.Next() {
		var r AuditListRow
		var detail, traceID, taskID, upID string
		if err := rows.Scan(&r.ID, &r.TS, &traceID, &taskID, &upID, &r.Event, &detail); err != nil {
			return nil, fmt.Errorf("scan audit row: %w", err)
		}
		r.TraceID = traceID
		r.HubTaskID = taskID
		r.UpstreamID = upID
		r.DetailJSON = detail
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountAudit returns the total matching rows (for pagination).
func (s *Store) CountAudit(ctx context.Context, f AuditListFilter) (int, error) {
	query, args := buildAuditListQuery("COUNT", f)
	var n int
	if err := s.db.QueryRowContext(s.withCtx(ctx), query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count audit: %w", err)
	}
	return n, nil
}

// buildAuditListQuery assembles the SELECT or COUNT query for audit_log.
func buildAuditListQuery(mode string, f AuditListFilter) (string, []any) {
	var args []any
	var where []string

	if f.UpstreamID != "" {
		where = append(where, "upstream_id = ?")
		args = append(args, f.UpstreamID)
	}
	if f.HubTaskID != "" {
		where = append(where, "hub_task_id = ?")
		args = append(args, f.HubTaskID)
	}
	if f.TraceID != "" {
		where = append(where, "trace_id = ?")
		args = append(args, f.TraceID)
	}
	if f.Event != "" {
		where = append(where, "event = ?")
		args = append(args, string(f.Event))
	}

	whereClause := ""
	if len(where) > 0 {
		whereClause = " WHERE " + strings.Join(where, " AND ")
	}

	if mode == "COUNT" {
		return "SELECT COUNT(*) FROM audit_log" + whereClause, args
	}

	q := `SELECT id, ts, COALESCE(trace_id,''), COALESCE(hub_task_id,''),
	             COALESCE(upstream_id,''), event, COALESCE(detail_json,'')
	      FROM audit_log` + whereClause + ` ORDER BY id DESC`

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
