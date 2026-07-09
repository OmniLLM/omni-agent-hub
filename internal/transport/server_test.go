package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

type fakeFetcher struct{ card *a2a.AgentCard }

func (f *fakeFetcher) Fetch(context.Context, string, string, string) (*a2a.AgentCard, error) {
	return f.card, nil
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Fake upstream that returns a task on message/send.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req a2a.JSONRPCRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		task := a2a.Task{TaskID: "up-1", ContextID: "c-1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}
		writeResp(w, req.ID, task)
	}))
	t.Cleanup(upstream.Close)

	reg := registry.New(db, &fakeFetcher{card: &a2a.AgentCard{
		Name: "hermes", URL: upstream.URL,
		Skills: []a2a.AgentSkill{{ID: "coding", Name: "Coding"}},
	}})
	u, _ := reg.Add(context.Background(), registry.AddInput{Name: "hermes", BaseURL: upstream.URL})
	reg.RecordSuccess(u.ID)

	cfg := &config.Config{
		Server: config.ServerConfig{APIKey: "client-key", AdminKey: "admin-key", PublicURL: "http://hub"},
		Hub:    config.HubConfig{Name: "Test Hub"},
	}
	cb := card.Start(context.Background(), reg, card.FromConfig(cfg, "test"))
	// Force initial rebuild since debounce goroutine has 100ms window.
	cb.Rebuild()

	d := dispatch.New(reg, db)
	return New(Deps{Cfg: cfg, Reg: reg, Card: cb, Store: db, Unary: d, Stream: d, Version: "test"})
}

func writeResp(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a2a.JSONRPCResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func TestAgentCard_Public(t *testing.T) {
	s := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/.well-known/agent-card.json", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d", rr.Code)
	}
	var c a2a.AgentCard
	if err := json.Unmarshal(rr.Body.Bytes(), &c); err != nil {
		t.Fatalf("parse card: %v", err)
	}
	if c.Name != "Test Hub" {
		t.Errorf("name = %q", c.Name)
	}
}

func TestClientAuth_Required(t *testing.T) {
	s := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("expected 401 without auth, got %d", rr.Code)
	}
}

func TestAdminSurface_SeparateKey(t *testing.T) {
	s := newTestServer(t)

	// Admin endpoint requires admin_key, not client key.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/upstreams", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("client key should be rejected on /admin, got %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	req.Header.Set("Authorization", "Bearer admin-key")
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("admin key should succeed, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminUpsertUpstream_UpdatesExistingByName(t *testing.T) {
	s := newTestServer(t)

	body := `{"name":"hermes","base_url":"http://127.0.0.1:1423","prefix":"@hermes","auth":{"scheme":"bearer","token":"tok"}}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/upstreams/upsert", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin-key")
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected update status 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/admin/upstreams/hermes", nil)
	req.Header.Set("Authorization", "Bearer admin-key")
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("inspect status = %d body=%s", rr.Code, rr.Body.String())
	}

	var detail map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatalf("parse detail: %v", err)
	}
	if detail["base_url"] != "http://127.0.0.1:1423" {
		t.Fatalf("base_url was not updated: %#v", detail["base_url"])
	}
}

func TestMessageSend_EndToEnd(t *testing.T) {
	s := newTestServer(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"skillId":"hermes.coding","message":{"role":"user","parts":[{"text":"hi"}]}}}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer client-key")
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp a2a.JSONRPCResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("rpc error: %+v", resp.Error)
	}
	// Task id should have been rewritten (not "up-1").
	task, _ := resp.Result.(map[string]any)
	if task == nil {
		t.Fatalf("no task in result")
	}
	if task["id"] == "up-1" {
		t.Errorf("task id was not rewritten")
	}
}
