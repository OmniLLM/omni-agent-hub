package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
	"github.com/OmniLLM/omni-agent-hub/internal/registry"
	"github.com/OmniLLM/omni-agent-hub/internal/router"
	"github.com/OmniLLM/omni-agent-hub/internal/store"
)

type fakeFetcher struct{ card *a2a.AgentCard }

func (f *fakeFetcher) Fetch(context.Context, string, string, string) (*a2a.AgentCard, error) {
	return f.card, nil
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// upstreamHandler returns an http.Handler that speaks minimal JSON-RPC for
// message/send and tasks/get.
type upstreamHandler struct {
	returnedTaskID string
	state          a2a.TaskState
	tHelper        *testing.T
}

func (u *upstreamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req a2a.JSONRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	task := a2a.Task{
		TaskID:    u.returnedTaskID,
		ContextID: "ctx-upstream",
		Status:    a2a.TaskStatus{State: u.state},
	}
	resp := a2a.JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: task}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func setupWithUpstream(t *testing.T, handler http.Handler) (*Dispatcher, registry.Upstream) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	db := openStore(t)
	reg := registry.New(db, &fakeFetcher{
		card: &a2a.AgentCard{Name: "fake", URL: srv.URL},
	})
	u, err := reg.Add(context.Background(), registry.AddInput{
		Name: "fake", BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("Add upstream: %v", err)
	}
	// Make it healthy explicitly (RecordSuccess).
	reg.RecordSuccess(u.ID)
	d := New(reg, db)
	return d, u
}

func TestSendMessage_RewritesTaskID(t *testing.T) {
	d, u := setupWithUpstream(t, &upstreamHandler{
		returnedTaskID: "upstream-task-42",
		state:          a2a.TaskStateCompleted,
	})
	req := UnaryRequest{
		Res:     router.Resolution{UpstreamID: u.ID, Reason: router.ReasonSkill},
		Message: a2a.Message{Role: a2a.RoleUser, Parts: []a2a.Part{{Text: "hi"}}},
	}
	resp, err := d.SendMessage(context.Background(), req)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if resp.Task == nil {
		t.Fatalf("nil task")
	}
	if resp.Task.TaskID == "upstream-task-42" {
		t.Fatalf("task id was NOT rewritten (still upstream id)")
	}
	// GetTask against the same hub id must return the same rewritten id.
	got, err := d.GetTask(context.Background(), resp.Task.TaskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.TaskID != resp.Task.TaskID {
		t.Fatalf("GetTask id mismatch: got %q want %q", got.TaskID, resp.Task.TaskID)
	}
}

func TestSendMessage_BreakerFailsFast(t *testing.T) {
	// Upstream that always 500s.
	fail := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	})
	d, u := setupWithUpstream(t, fail)
	req := UnaryRequest{
		Res:     router.Resolution{UpstreamID: u.ID, Reason: router.ReasonSkill},
		Message: a2a.Message{Role: a2a.RoleUser, Parts: []a2a.Part{{Text: "hi"}}},
	}
	// Send 3 times to trip the breaker (each records a failure).
	for i := 0; i < 3; i++ {
		_, err := d.SendMessage(context.Background(), req)
		if err == nil {
			t.Fatalf("call %d expected error", i+1)
		}
	}
	// 4th call should be fail-fast (breaker open) — a distinctive JSON-RPC
	// error code -32010.
	_, err := d.SendMessage(context.Background(), req)
	if err == nil {
		t.Fatalf("expected breaker error")
	}
	jrpcErr, ok := err.(*a2a.JSONRPCError)
	if !ok {
		t.Fatalf("expected *a2a.JSONRPCError, got %T: %v", err, err)
	}
	if jrpcErr.Code != a2a.ErrUnavailable {
		t.Errorf("expected ErrUnavailable, got %d", jrpcErr.Code)
	}
}

func TestGetTask_NotFound(t *testing.T) {
	d, _ := setupWithUpstream(t, &upstreamHandler{})
	_, err := d.GetTask(context.Background(), "does-not-exist")
	jrpcErr, ok := err.(*a2a.JSONRPCError)
	if !ok || jrpcErr.Code != a2a.ErrTaskNotFound {
		t.Fatalf("expected ErrTaskNotFound, got %v", err)
	}
}

func TestSendMessage_NonJSONUpstreamIncludesBodySnippet(t *testing.T) {
	bad := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("Not Found"))
	})
	d, u := setupWithUpstream(t, bad)
	req := UnaryRequest{
		Res:     router.Resolution{UpstreamID: u.ID, Reason: router.ReasonSkill},
		Message: a2a.Message{Role: a2a.RoleUser, Parts: []a2a.Part{{Text: "hi"}}},
	}
	_, err := d.SendMessage(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error")
	}
	jrpcErr, ok := err.(*a2a.JSONRPCError)
	if !ok {
		t.Fatalf("expected *a2a.JSONRPCError, got %T: %v", err, err)
	}
	if jrpcErr.Code != a2a.ErrUpstreamHTTP {
		t.Fatalf("expected ErrUpstreamHTTP, got %d", jrpcErr.Code)
	}
	data, ok := jrpcErr.Data.(string)
	if !ok {
		t.Fatalf("expected string error data, got %T", jrpcErr.Data)
	}
	if data != "upstream returned HTTP 404: Not Found" {
		t.Fatalf("unexpected error data: %q", data)
	}
}

func TestSendMessage_4xxDoesNotTripBreaker(t *testing.T) {
	// A 404 from upstream is a client error — the breaker should NOT trip,
	// so a 4th call must NOT get ErrUnavailable.
	notFound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("Not Found"))
	})
	d, u := setupWithUpstream(t, notFound)
	req := UnaryRequest{
		Res:     router.Resolution{UpstreamID: u.ID, Reason: router.ReasonSkill},
		Message: a2a.Message{Role: a2a.RoleUser, Parts: []a2a.Part{{Text: "hi"}}},
	}
	// Send 4 times — all should get ErrUpstreamHTTP, never ErrUnavailable.
	for i := 0; i < 4; i++ {
		_, err := d.SendMessage(context.Background(), req)
		if err == nil {
			t.Fatalf("call %d: expected error", i+1)
		}
		jrpcErr, ok := err.(*a2a.JSONRPCError)
		if !ok {
			t.Fatalf("call %d: expected *a2a.JSONRPCError, got %T: %v", i+1, err, err)
		}
		if jrpcErr.Code == a2a.ErrUnavailable {
			t.Fatalf("call %d: 4xx should NOT trip the breaker, but got ErrUnavailable", i+1)
		}
		if jrpcErr.Code != a2a.ErrUpstreamHTTP {
			t.Fatalf("call %d: expected ErrUpstreamHTTP, got %d", i+1, jrpcErr.Code)
		}
	}
}
