package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpen_MigratesToV1(t *testing.T) {
	s := openTestStore(t)
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if v != currentSchemaVersion {
		t.Fatalf("user_version = %d, want %d", v, currentSchemaVersion)
	}
	// Reopening the same file must not re-run migrations (idempotent).
	_ = s.Close()
	s2, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	s2.Close()
}

func TestUpsertUpstream_RoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	in := UpstreamRow{
		ID: "u-1", Name: "hermes", BaseURL: "http://x", AuthScheme: "bearer",
		AuthToken: "t", Prefix: "@hermes", Enabled: true,
		Source: SourceConfig, Status: StatusUnknown,
	}
	if err := s.UpsertUpstream(ctx, in); err != nil {
		t.Fatalf("UpsertUpstream: %v", err)
	}
	out, err := s.GetUpstreamByName(ctx, "hermes")
	if err != nil {
		t.Fatalf("GetUpstreamByName: %v", err)
	}
	if out.BaseURL != "http://x" || out.AuthToken != "t" || !out.Enabled {
		t.Errorf("round-trip mismatch: %+v", out)
	}
	// Config→Admin: admin source should stick even if we later re-upsert as config.
	adminRow := in
	adminRow.Source = SourceAdmin
	if err := s.UpsertUpstream(ctx, adminRow); err != nil {
		t.Fatalf("upsert as admin: %v", err)
	}
	// Now re-upsert as config; source should stay admin.
	if err := s.UpsertUpstream(ctx, in); err != nil {
		t.Fatalf("re-upsert as config: %v", err)
	}
	after, _ := s.GetUpstreamByName(ctx, "hermes")
	if after.Source != SourceAdmin {
		t.Errorf("expected admin source to stick, got %s", after.Source)
	}
}

func TestTasks_MapAndLookup(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Upstream row first (task FK'd to it)
	_ = s.UpsertUpstream(ctx, UpstreamRow{
		ID: "u-1", Name: "hermes", BaseURL: "http://x",
		AuthScheme: "bearer", Source: SourceConfig, Status: StatusHealthy,
	})

	if err := s.CreateTask(ctx, "hub-t1", "ctx-1", "u-1"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := s.MapTaskID(ctx, "hub-t1", "u-1", "up-task-a"); err != nil {
		t.Fatalf("MapTaskID: %v", err)
	}
	got, err := s.LookupUpstreamTaskID(ctx, "hub-t1")
	if err != nil || got != "up-task-a" {
		t.Fatalf("LookupUpstreamTaskID = %q err=%v", got, err)
	}

	// Sticky context lookup returns the upstream while task is non-terminal.
	up, ok := s.LookupContext(ctx, "ctx-1")
	if !ok || up != "u-1" {
		t.Fatalf("LookupContext(ctx-1) = (%q,%v)", up, ok)
	}
	// After marking terminal, stickiness should drop.
	_ = s.UpdateTaskSnapshot(ctx, "hub-t1", a2a.TaskStateCompleted, &a2a.Task{TaskID: "hub-t1"})
	_, ok = s.LookupContext(ctx, "ctx-1")
	if ok {
		t.Fatalf("LookupContext should be empty after terminal state")
	}
}

func TestAudit_WriteAndVacuum(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for i := 0; i < 25; i++ {
		if err := s.WriteAudit(ctx, AuditEntry{
			TraceID: "tr", Event: EventSend, Detail: map[string]int{"i": i},
		}); err != nil {
			t.Fatalf("WriteAudit: %v", err)
		}
	}
	if err := s.VacuumAudit(ctx, 10); err != nil {
		t.Fatalf("VacuumAudit: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 10 {
		t.Errorf("count after vacuum = %d, want 10", n)
	}
}

// --- New tests for ListTasks, CountTasksByState, GetTaskDetail, ListAudit, CountAudit ---

func seedUpstreamAndTasks(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()

	_ = s.UpsertUpstream(ctx, UpstreamRow{
		ID: "u-1", Name: "hermes", BaseURL: "http://x",
		AuthScheme: "bearer", Source: SourceConfig, Status: StatusHealthy,
	})
	_ = s.UpsertUpstream(ctx, UpstreamRow{
		ID: "u-2", Name: "scribe", BaseURL: "http://y",
		AuthScheme: "none", Source: SourceAdmin, Status: StatusUnhealthy,
	})

	// Create 5 tasks: 2 active, 3 terminal.
	for i, tc := range []struct {
		id, ctx, up string
		state       a2a.TaskState
	}{
		{"t-1", "ctx-a", "u-1", a2a.TaskStateWorking},
		{"t-2", "ctx-b", "u-1", a2a.TaskStateSubmitted},
		{"t-3", "ctx-c", "u-2", a2a.TaskStateCompleted},
		{"t-4", "ctx-d", "u-1", a2a.TaskStateFailed},
		{"t-5", "ctx-e", "u-2", a2a.TaskStateInputRequired},
	} {
		if err := s.CreateTask(ctx, tc.id, tc.ctx, tc.up); err != nil {
			t.Fatalf("CreateTask(%d): %v", i, err)
		}
		if tc.state != a2a.TaskStateSubmitted {
			_ = s.UpdateTaskSnapshot(ctx, tc.id, tc.state, nil)
		}
	}
	// Map upstream task ID for t-1.
	_ = s.MapTaskID(ctx, "t-1", "u-1", "upstream-task-abc")
}

func TestListTasks_DefaultActiveOnly(t *testing.T) {
	s := openTestStore(t)
	seedUpstreamAndTasks(t, s)

	tasks, err := s.ListTasks(context.Background(), TaskListFilter{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	// Active = working, submitted, input-required → 3 tasks.
	if len(tasks) != 3 {
		t.Fatalf("got %d tasks, want 3 (active only)", len(tasks))
	}
	for _, tk := range tasks {
		if tk.State == a2a.TaskStateCompleted || tk.State == a2a.TaskStateFailed || tk.State == a2a.TaskStateCanceled {
			t.Errorf("unexpected terminal state %s in active-only results", tk.State)
		}
	}
}

func TestListTasks_RecentIncludesTerminal(t *testing.T) {
	s := openTestStore(t)
	seedUpstreamAndTasks(t, s)

	tasks, err := s.ListTasks(context.Background(), TaskListFilter{Recent: true})
	if err != nil {
		t.Fatalf("ListTasks(recent): %v", err)
	}
	if len(tasks) != 5 {
		t.Fatalf("got %d tasks, want 5 (all)", len(tasks))
	}
}

func TestListTasks_FilterByState(t *testing.T) {
	s := openTestStore(t)
	seedUpstreamAndTasks(t, s)

	tasks, err := s.ListTasks(context.Background(), TaskListFilter{
		States: []a2a.TaskState{a2a.TaskStateFailed},
	})
	if err != nil {
		t.Fatalf("ListTasks(failed): %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1 (failed only)", len(tasks))
	}
	if tasks[0].HubTaskID != "t-4" {
		t.Errorf("expected t-4, got %s", tasks[0].HubTaskID)
	}
}

func TestListTasks_FilterByUpstreamID(t *testing.T) {
	s := openTestStore(t)
	seedUpstreamAndTasks(t, s)

	tasks, err := s.ListTasks(context.Background(), TaskListFilter{
		UpstreamID: "u-2",
		Recent:     true,
	})
	if err != nil {
		t.Fatalf("ListTasks(u-2): %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2 for u-2", len(tasks))
	}
}

func TestListTasks_JoinsUpstreamTaskID(t *testing.T) {
	s := openTestStore(t)
	seedUpstreamAndTasks(t, s)

	tasks, err := s.ListTasks(context.Background(), TaskListFilter{
		States: []a2a.TaskState{a2a.TaskStateWorking},
	})
	if err != nil {
		t.Fatalf("ListTasks(working): %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(tasks))
	}
	if tasks[0].UpstreamTaskID != "upstream-task-abc" {
		t.Errorf("expected upstream-task-abc, got %q", tasks[0].UpstreamTaskID)
	}
}

func TestCountTasks(t *testing.T) {
	s := openTestStore(t)
	seedUpstreamAndTasks(t, s)

	n, err := s.CountTasks(context.Background(), TaskListFilter{})
	if err != nil {
		t.Fatalf("CountTasks: %v", err)
	}
	if n != 3 {
		t.Fatalf("CountTasks = %d, want 3 (active)", n)
	}

	all, err := s.CountTasks(context.Background(), TaskListFilter{Recent: true})
	if err != nil {
		t.Fatalf("CountTasks(recent): %v", err)
	}
	if all != 5 {
		t.Fatalf("CountTasks(recent) = %d, want 5", all)
	}
}

func TestCountTasksByState(t *testing.T) {
	s := openTestStore(t)
	seedUpstreamAndTasks(t, s)

	c, err := s.CountTasksByState(context.Background())
	if err != nil {
		t.Fatalf("CountTasksByState: %v", err)
	}
	if c.Working != 1 {
		t.Errorf("working = %d, want 1", c.Working)
	}
	if c.Submitted != 1 {
		t.Errorf("submitted = %d, want 1", c.Submitted)
	}
	if c.Completed != 1 {
		t.Errorf("completed = %d, want 1", c.Completed)
	}
	if c.Failed != 1 {
		t.Errorf("failed = %d, want 1", c.Failed)
	}
	if c.InputRequired != 1 {
		t.Errorf("input_required = %d, want 1", c.InputRequired)
	}
	if c.Total != 5 {
		t.Errorf("total = %d, want 5", c.Total)
	}
}

func TestGetTaskDetail(t *testing.T) {
	s := openTestStore(t)
	seedUpstreamAndTasks(t, s)

	detail, err := s.GetTaskDetail(context.Background(), "t-1")
	if err != nil {
		t.Fatalf("GetTaskDetail: %v", err)
	}
	if detail.Task.HubTaskID != "t-1" {
		t.Errorf("task id = %s, want t-1", detail.Task.HubTaskID)
	}
	if detail.UpstreamTaskID != "upstream-task-abc" {
		t.Errorf("upstream_task_id = %q, want upstream-task-abc", detail.UpstreamTaskID)
	}
	if detail.Task.State != a2a.TaskStateWorking {
		t.Errorf("state = %s, want working", detail.Task.State)
	}
}

func TestGetTaskDetail_NotFound(t *testing.T) {
	s := openTestStore(t)
	_, err := s.GetTaskDetail(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
}

func TestListAudit_BasicAndFilters(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Write a mix of audit events.
	for _, e := range []AuditEntry{
		{TraceID: "tr-1", HubTaskID: "t-1", UpstreamID: "u-1", Event: EventSend},
		{TraceID: "tr-1", HubTaskID: "t-1", UpstreamID: "u-1", Event: EventResponse},
		{TraceID: "tr-2", HubTaskID: "t-2", UpstreamID: "u-2", Event: EventError, Detail: map[string]string{"err": "timeout"}},
		{TraceID: "tr-3", HubTaskID: "t-1", UpstreamID: "u-1", Event: EventStreamStart},
	} {
		if err := s.WriteAudit(ctx, e); err != nil {
			t.Fatalf("WriteAudit: %v", err)
		}
	}

	// Unfiltered.
	all, err := s.ListAudit(ctx, AuditListFilter{})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("got %d entries, want 4", len(all))
	}
	// Newest first (stream-start should be first).
	if all[0].Event != EventStreamStart {
		t.Errorf("first event = %s, want stream-start", all[0].Event)
	}

	// Filter by upstream.
	byUp, err := s.ListAudit(ctx, AuditListFilter{UpstreamID: "u-2"})
	if err != nil {
		t.Fatalf("ListAudit(u-2): %v", err)
	}
	if len(byUp) != 1 {
		t.Fatalf("got %d, want 1 for u-2", len(byUp))
	}

	// Filter by event.
	byEvt, err := s.ListAudit(ctx, AuditListFilter{Event: EventSend})
	if err != nil {
		t.Fatalf("ListAudit(send): %v", err)
	}
	if len(byEvt) != 1 {
		t.Fatalf("got %d, want 1 for event=send", len(byEvt))
	}

	// Filter by task.
	byTask, err := s.ListAudit(ctx, AuditListFilter{HubTaskID: "t-1"})
	if err != nil {
		t.Fatalf("ListAudit(t-1): %v", err)
	}
	if len(byTask) != 3 {
		t.Fatalf("got %d, want 3 for task=t-1", len(byTask))
	}
}

func TestCountAudit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for i := 0; i < 15; i++ {
		_ = s.WriteAudit(ctx, AuditEntry{Event: EventSend})
	}

	n, err := s.CountAudit(ctx, AuditListFilter{})
	if err != nil {
		t.Fatalf("CountAudit: %v", err)
	}
	if n != 15 {
		t.Errorf("CountAudit = %d, want 15", n)
	}

	// With filter.
	_ = s.WriteAudit(ctx, AuditEntry{Event: EventError, UpstreamID: "u-x"})
	n2, err := s.CountAudit(ctx, AuditListFilter{UpstreamID: "u-x"})
	if err != nil {
		t.Fatalf("CountAudit(u-x): %v", err)
	}
	if n2 != 1 {
		t.Errorf("CountAudit(u-x) = %d, want 1", n2)
	}
}

func TestListTasks_Pagination(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_ = s.UpsertUpstream(ctx, UpstreamRow{
		ID: "u-1", Name: "test", BaseURL: "http://x",
		AuthScheme: "none", Source: SourceConfig, Status: StatusHealthy,
	})
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("t-%02d", i)
		_ = s.CreateTask(ctx, id, "ctx", "u-1")
	}

	page1, _ := s.ListTasks(ctx, TaskListFilter{Limit: 3, Offset: 0})
	page2, _ := s.ListTasks(ctx, TaskListFilter{Limit: 3, Offset: 3})
	if len(page1) != 3 || len(page2) != 3 {
		t.Fatalf("pagination: page1=%d page2=%d, want 3,3", len(page1), len(page2))
	}
	// Pages shouldn't overlap.
	if page1[0].HubTaskID == page2[0].HubTaskID {
		t.Errorf("pages overlap: both start with %s", page1[0].HubTaskID)
	}
}
