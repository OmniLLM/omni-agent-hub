package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
	"github.com/OmniLLM/omni-agent-hub/internal/card"
	"github.com/OmniLLM/omni-agent-hub/internal/config"
	"github.com/OmniLLM/omni-agent-hub/internal/dispatch"
	"github.com/OmniLLM/omni-agent-hub/internal/registry"
	"github.com/OmniLLM/omni-agent-hub/internal/store"
)

// TestRESTMessageSend_EndToEnd exercises the A2A HTTP+JSON binding: a bare
// MessageSendParams body in, a raw Task object out (no JSON-RPC envelope).
func TestRESTMessageSend_EndToEnd(t *testing.T) {
	s := newTestServer(t)
	body := `{"skillId":"hermes.coding","message":{"role":"user","parts":[{"text":"hi"}]}}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/a2a/v1/message:send", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer client-key")
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := m["jsonrpc"]; ok {
		t.Errorf("REST response must not be JSON-RPC-wrapped: %s", rr.Body.String())
	}
	if m["id"] == nil {
		t.Fatalf("expected raw task with id, got %s", rr.Body.String())
	}
	if m["id"] == "up-1" {
		t.Errorf("task id was not rewritten")
	}
}

// TestRESTMessageSend_AuthRequired confirms the REST surface is behind clientAuth.
func TestRESTMessageSend_AuthRequired(t *testing.T) {
	s := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/a2a/v1/message:send",
		strings.NewReader(`{"message":{"parts":[{"text":"hi"}]}}`))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("expected 401 without auth, got %d", rr.Code)
	}
}

// TestRESTMessageSend_NoRoute confirms an unroutable request returns a plain
// HTTP error (not a JSON-RPC error envelope).
func TestRESTMessageSend_NoRoute(t *testing.T) {
	s := newTestServer(t)
	// No skillId, no @mention, no prefix → nothing to route on.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/a2a/v1/message:send",
		strings.NewReader(`{"message":{"parts":[{"text":"nope"}]}}`))
	req.Header.Set("Authorization", "Bearer client-key")
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", rr.Code, rr.Body.String())
	}
	var m map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &m)
	if _, ok := m["jsonrpc"]; ok {
		t.Errorf("no-route error must not be JSON-RPC-wrapped")
	}
	if m["error"] == nil {
		t.Errorf("expected error body, got %s", rr.Body.String())
	}
}

// TestRESTMessageStream_EndToEnd confirms /a2a/v1/message:stream relays raw A2A
// events as SSE frames (not JSON-RPC-wrapped).
func TestRESTMessageStream_EndToEnd(t *testing.T) {
	s := newStreamTestServer(t)
	body := `{"skillId":"fake.chat","message":{"role":"user","parts":[{"text":"hi"}]}}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/a2a/v1/message:stream", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer client-key")
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}
	out := rr.Body.String()
	if !strings.Contains(out, "data: ") {
		t.Fatalf("expected SSE data frames, got %s", out)
	}
	if strings.Contains(out, `"jsonrpc"`) {
		t.Errorf("SSE frames must not be JSON-RPC-wrapped: %s", out)
	}
	if !strings.Contains(out, `"completed"`) {
		t.Errorf("expected terminal completed event, got %s", out)
	}
}

// TestLegacyMessageSendCompat_Unchanged is a regression guard: adding the REST
// binding must not alter the pre-existing /message:send compat shim.
func TestLegacyMessageSendCompat_Unchanged(t *testing.T) {
	s := newTestServer(t)
	body := `{"tool":"hermes.coding","messages":[{"role":"user","parts":[{"text":"hi"}]}]}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/message:send", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer client-key")
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("legacy /message:send broke: status=%d body=%s", rr.Code, rr.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m["id"] == "up-1" {
		t.Errorf("task id was not rewritten")
	}
}

func TestRESTTaskEndpoints(t *testing.T) {
	s := newTestServer(t)
	taskID := createRESTTask(t, s)

	t.Run("get returns raw task", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/a2a/v1/tasks/"+taskID, nil)
		req.Header.Set("Authorization", "Bearer client-key")
		s.Handler().ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
		}
		var task a2a.Task
		if err := json.Unmarshal(rr.Body.Bytes(), &task); err != nil {
			t.Fatalf("parse task: %v", err)
		}
		if task.TaskID != taskID {
			t.Fatalf("task id = %q, want %q", task.TaskID, taskID)
		}
	})

	t.Run("cancel returns ack", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/a2a/v1/tasks/"+taskID+":cancel", nil)
		req.Header.Set("Authorization", "Bearer client-key")
		s.Handler().ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
		}
		var body map[string]string
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("parse cancel ack: %v", err)
		}
		if body["status"] != "canceled" {
			t.Fatalf("status = %q, want canceled", body["status"])
		}
	})
}

func TestRESTTaskGet_NotFound(t *testing.T) {
	s := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/a2a/v1/tasks/missing", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	assertRESTErrorCode(t, rr, a2a.ErrTaskNotFound)
}

func TestRESTMessageSend_InvalidParams(t *testing.T) {
	s := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/a2a/v1/message:send",
		strings.NewReader(`{"message":{"parts":[]}}`))
	req.Header.Set("Authorization", "Bearer client-key")
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
	assertRESTErrorCode(t, rr, a2a.ErrInvalidParams)
}

func TestRESTTaskAction_UnknownAction(t *testing.T) {
	s := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/a2a/v1/tasks/task-1:pause", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
	assertRESTErrorCode(t, rr, a2a.ErrMethodNotFound)
}

func createRESTTask(t *testing.T, s *Server) string {
	t.Helper()

	body := `{"skillId":"hermes.coding","message":{"role":"user","parts":[{"text":"hi"}]}}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/a2a/v1/message:send", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer client-key")
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("create task status = %d body=%s", rr.Code, rr.Body.String())
	}
	var task a2a.Task
	if err := json.Unmarshal(rr.Body.Bytes(), &task); err != nil {
		t.Fatalf("parse created task: %v", err)
	}
	if task.TaskID == "" {
		t.Fatalf("created task id is empty: %s", rr.Body.String())
	}
	return task.TaskID
}

func assertRESTErrorCode(t *testing.T, rr *httptest.ResponseRecorder, want int) {
	t.Helper()

	var body struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse error body: %v", err)
	}
	if body.Error.Code != want {
		t.Fatalf("error code = %d, want %d; body=%s", body.Error.Code, want, rr.Body.String())
	}
}

// newStreamTestServer is like newTestServer but its fake upstream speaks SSE,
// emitting a single terminal (completed) event — used to drive the stream path.
func newStreamTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"id":"up-1","contextId":"c-1","status":{"state":"completed"},"final":true}` + "\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	reg := registry.New(db, &fakeFetcher{card: &a2a.AgentCard{
		Name: "fake", URL: upstream.URL,
		Capabilities: a2a.AgentCapabilities{Streaming: true},
		Skills:       []a2a.AgentSkill{{ID: "chat", Name: "Chat"}},
	}})
	u, _ := reg.Add(context.Background(), registry.AddInput{Name: "fake", BaseURL: upstream.URL})
	reg.RecordSuccess(u.ID)

	cfg := &config.Config{
		Server: config.ServerConfig{APIKey: "client-key", AdminKey: "admin-key", PublicURL: "http://hub"},
		Hub:    config.HubConfig{Name: "Test Hub"},
	}
	cb := card.Start(context.Background(), reg, card.FromConfig(cfg, "test"))
	cb.Rebuild()

	d := dispatch.New(reg, db)
	return New(Deps{Cfg: cfg, Reg: reg, Card: cb, Store: db, Unary: d, Stream: d, Version: "test"})
}
