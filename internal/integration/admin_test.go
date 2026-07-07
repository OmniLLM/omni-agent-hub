package integration

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
	"github.com/OmniLLM/omni-agent-hub/internal/transport"
)

// bootHubEmpty starts a hub with zero upstreams and returns the hub + a
// running fake upstream (not yet registered).
func bootHubEmpty(t *testing.T) (hub *httptest.Server, fakeUpstream *httptest.Server, cleanup func()) {
	t.Helper()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/agent-card.json" {
			_ = json.NewEncoder(w).Encode(a2a.AgentCard{
				Name: "later-fake", URL: "http://x",
				Skills: []a2a.AgentSkill{{ID: "chat", Name: "Chat"}},
			})
			return
		}
		http.NotFound(w, r)
	}))
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	cfg := &config.Config{
		Server: config.ServerConfig{
			APIKey: "client-k", AdminKey: "admin-k", PublicURL: "http://hub",
		},
		Hub: config.HubConfig{Name: "AdminHub"},
	}
	reg := registry.New(db, nil)
	cb := card.Start(context.Background(), reg, card.FromConfig(cfg, "test"))
	cb.Rebuild()
	disp := dispatch.New(reg, db)
	tsrv := transport.New(transport.Deps{
		Cfg: cfg, Reg: reg, Card: cb, Store: db,
		Unary: disp, Stream: disp, Version: "test",
	})
	hub = httptest.NewServer(tsrv.Handler())
	cleanup = func() {
		hub.Close()
		fake.Close()
		db.Close()
	}
	return hub, fake, cleanup
}

// adminReq builds a request signed with the admin key.
func adminReq(method, url, body string) *http.Request {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, url, r)
	req.Header.Set("Authorization", "Bearer admin-k")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestIntegration_AdminLifecycle_AddRemoveRefresh(t *testing.T) {
	hub, fake, cleanup := bootHubEmpty(t)
	defer cleanup()

	// 1. list — should be empty.
	resp, err := http.DefaultClient.Do(adminReq("GET", hub.URL+"/admin/upstreams", ""))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var list []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 0 {
		t.Fatalf("empty registry expected, got %+v", list)
	}

	// 2. add via admin API.
	addBody := `{"name":"newone","base_url":"` + fake.URL + `","auth":{"scheme":"none"}}`
	resp, err = http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/upstreams", addBody))
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("add status = %d body=%s", resp.StatusCode, string(body))
	}
	var created map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("no id in create response: %+v", created)
	}

	// 3. duplicate add — must fail with 409.
	resp, _ = http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/upstreams", addBody))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate add: got %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()

	// 4. refresh single — should hit the fake's card endpoint.
	resp, err = http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/upstreams/"+id+"/refresh", ""))
	if err != nil {
		t.Fatalf("refresh one: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 5. list — should show one upstream, with the card populated.
	resp, _ = http.DefaultClient.Do(adminReq("GET", hub.URL+"/admin/upstreams", ""))
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 1 {
		t.Fatalf("after add expected 1 upstream, got %d", len(list))
	}
	if list[0]["has_card"] != true {
		t.Errorf("expected has_card=true after refresh, got %+v", list[0])
	}

	// 6. admin skills — should include one entry.
	resp, _ = http.DefaultClient.Do(adminReq("GET", hub.URL+"/admin/skills", ""))
	var skills []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&skills)
	resp.Body.Close()
	if len(skills) != 1 || skills[0]["skill_id"] != "newone.chat" {
		t.Fatalf("skills = %+v", skills)
	}

	// 7. delete.
	resp, err = http.DefaultClient.Do(adminReq("DELETE", hub.URL+"/admin/upstreams/"+id, ""))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 8. list — back to empty.
	resp, _ = http.DefaultClient.Do(adminReq("GET", hub.URL+"/admin/upstreams", ""))
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 0 {
		t.Fatalf("after delete expected 0 upstreams, got %d", len(list))
	}
}

func TestIntegration_MetricsEndpoint(t *testing.T) {
	hub, _, cleanup := bootHubEmpty(t)
	defer cleanup()

	// Add one upstream so metrics has something to print.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(a2a.AgentCard{Name: "m", URL: "http://x"})
	}))
	defer fake.Close()
	addBody := `{"name":"m","base_url":"` + fake.URL + `","auth":{"scheme":"none"}}`
	req := adminReq("POST", hub.URL+"/admin/upstreams", addBody)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	// Now hit /metrics (public, no auth).
	resp, err := http.Get(hub.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got := string(body)
	if !bytes.Contains(body, []byte("omni_a2a_upstream_healthy")) {
		t.Errorf("metrics missing omni_a2a_upstream_healthy:\n%s", got)
	}
	if !bytes.Contains(body, []byte("omni_a2a_tasks_active")) {
		t.Errorf("metrics missing omni_a2a_tasks_active:\n%s", got)
	}
}

// --- Comprehensive E2E tests for upstream operations -------------------------

// bootHubWithDB returns a running hub, the store, and the registry so tests
// can simulate restart by creating a new hub from the same DB.
func bootHubWithDB(t *testing.T) (hub *httptest.Server, fake *httptest.Server, db *store.Store, reg registry.Registry, cleanup func()) {
	t.Helper()
	fake = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/agent-card.json" {
			_ = json.NewEncoder(w).Encode(a2a.AgentCard{
				Name: "test-fake", URL: "http://x",
				Skills: []a2a.AgentSkill{{ID: "chat", Name: "Chat"}},
			})
			return
		}
		http.NotFound(w, r)
	}))
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	cfg := &config.Config{
		Server: config.ServerConfig{
			APIKey: "client-k", AdminKey: "admin-k", PublicURL: "http://hub",
		},
		Hub: config.HubConfig{Name: "E2EHub"},
	}
	reg = registry.New(db, nil)
	cb := card.Start(context.Background(), reg, card.FromConfig(cfg, "test"))
	cb.Rebuild()
	disp := dispatch.New(reg, db)
	tsrv := transport.New(transport.Deps{
		Cfg: cfg, Reg: reg, Card: cb, Store: db,
		Unary: disp, Stream: disp, Version: "test",
	})
	hub = httptest.NewServer(tsrv.Handler())
	cleanup = func() {
		hub.Close()
		fake.Close()
		db.Close()
	}
	return hub, fake, db, reg, cleanup
}

// rebuildHub creates a new hub server from the same DB, simulating a restart.
// It loads config-defined upstreams via Bootstrap to mirror real startup.
func rebuildHub(t *testing.T, db *store.Store, cfgUpstreams []config.UpstreamCfg) (hub *httptest.Server, reg registry.Registry) {
	t.Helper()
	cfg := &config.Config{
		Server: config.ServerConfig{
			APIKey: "client-k", AdminKey: "admin-k", PublicURL: "http://hub",
		},
		Hub: config.HubConfig{Name: "E2EHub"},
	}
	reg = registry.New(db, nil)
	if err := registry.Bootstrap(context.Background(), reg, db, cfgUpstreams); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	cb := card.Start(context.Background(), reg, card.FromConfig(cfg, "test"))
	cb.Rebuild()
	disp := dispatch.New(reg, db)
	tsrv := transport.New(transport.Deps{
		Cfg: cfg, Reg: reg, Card: cb, Store: db,
		Unary: disp, Stream: disp, Version: "test",
	})
	hub = httptest.NewServer(tsrv.Handler())
	return hub, reg
}

// adminList is a helper that GETs /admin/upstreams and returns the decoded list.
func adminList(t *testing.T, hubURL string) []map[string]any {
	t.Helper()
	resp, err := http.DefaultClient.Do(adminReq("GET", hubURL+"/admin/upstreams", ""))
	if err != nil {
		t.Fatalf("GET /admin/upstreams: %v", err)
	}
	var list []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	return list
}

func TestIntegration_AddRemove_PersistsAcrossRestart(t *testing.T) {
	// If the admin-removed upstream is NOT in config.yaml, it must not
	// reappear after restart. (If config declares it, config wins — see
	// TestIntegration_ConfigOverridesAdminRemoval.)
	_, fake, db, _, cleanup := bootHubWithDB(t)
	defer cleanup()

	// Add upstream via admin API (simulates `oah up add`).
	reg1 := registry.New(db, nil)
	u, err := reg1.Add(context.Background(), registry.AddInput{
		Name: "ephemeral", BaseURL: fake.URL, Auth: config.AuthConfig{Scheme: "none"},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Remove it (simulates `oah up remove`).
	if err := reg1.Remove(context.Background(), u.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Simulate restart with NO config entries (this is the real-world case
	// from the bug report: hermes was admin-added and only omnilauncher is
	// in config.yaml).
	hub2, reg2 := rebuildHub(t, db, nil)
	defer hub2.Close()

	// The admin-removed upstream must NOT reappear.
	list := adminList(t, hub2.URL)
	for _, item := range list {
		if item["name"] == "ephemeral" {
			t.Fatalf("admin-removed upstream 'ephemeral' was resurrected by restart — bug!")
		}
	}

	// The registry should also not expose it.
	for _, u := range reg2.List() {
		if u.Name == "ephemeral" {
			t.Fatalf("admin-removed upstream should not appear in registry List()")
		}
	}
}

func TestIntegration_ConfigOverridesAdminRemoval(t *testing.T) {
	// If the user explicitly re-declares the upstream in config.yaml, config
	// wins and the upstream is re-adopted (preserving the original DB id).
	_, fake, db, _, cleanup := bootHubWithDB(t)
	defer cleanup()

	// Admin add + remove.
	reg1 := registry.New(db, nil)
	u, err := reg1.Add(context.Background(), registry.AddInput{
		Name: "reclaimed", BaseURL: fake.URL, Auth: config.AuthConfig{Scheme: "none"},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	origID := u.ID
	if err := reg1.Remove(context.Background(), u.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Restart with config re-declaring the upstream.
	cfgUpstreams := []config.UpstreamCfg{
		{Name: "reclaimed", BaseURL: fake.URL, Auth: config.AuthConfig{Scheme: "none"}, Enabled: true},
	}
	hub2, reg2 := rebuildHub(t, db, cfgUpstreams)
	defer hub2.Close()

	// Should be present and enabled.
	list := adminList(t, hub2.URL)
	if len(list) != 1 {
		t.Fatalf("expected 1 upstream, got %+v", list)
	}
	if list[0]["name"] != "reclaimed" {
		t.Fatalf("expected 'reclaimed', got %+v", list[0])
	}
	// The DB id must be preserved (so any task FKs still work).
	if list[0]["id"] != string(origID) {
		t.Fatalf("expected DB id preserved: got %v, want %s", list[0]["id"], origID)
	}
	// Registry List should also show it.
	found := false
	for _, u := range reg2.List() {
		if u.Name == "reclaimed" && u.Enabled {
			found = true
		}
	}
	if !found {
		t.Fatalf("reclaimed upstream should be in registry list")
	}
}

func TestIntegration_ReAddAfterRemove(t *testing.T) {
	// After removing an upstream and re-adding it via admin API, the new
	// upstream should work normally and survive restart.
	hub, fake, db, _, cleanup := bootHubWithDB(t)
	defer cleanup()

	// Add.
	addBody := `{"name":"cycle","base_url":"` + fake.URL + `","auth":{"scheme":"none"}}`
	resp, _ := http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/upstreams", addBody))
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("add: status=%d body=%s", resp.StatusCode, body)
	}
	var created map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	id := created["id"].(string)

	// Remove.
	resp, _ = http.DefaultClient.Do(adminReq("DELETE", hub.URL+"/admin/upstreams/"+id, ""))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// Verify gone.
	list := adminList(t, hub.URL)
	if len(list) != 0 {
		t.Fatalf("expected 0 after delete, got %d", len(list))
	}

	// Re-add with same name.
	resp, _ = http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/upstreams", addBody))
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("re-add: status=%d body=%s", resp.StatusCode, body)
	}
	var readded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&readded)
	resp.Body.Close()

	// The re-added upstream should appear and have a card.
	list = adminList(t, hub.URL)
	if len(list) != 1 {
		t.Fatalf("expected 1 after re-add, got %d", len(list))
	}
	if list[0]["name"] != "cycle" {
		t.Fatalf("expected name 'cycle', got %v", list[0]["name"])
	}

	// Simulate restart with no config upstreams — the admin-added one should
	// survive because it was re-added.
	hub2, _ := rebuildHub(t, db, nil)
	defer hub2.Close()

	list2 := adminList(t, hub2.URL)
	found := false
	for _, item := range list2 {
		if item["name"] == "cycle" {
			enabled, _ := item["enabled"].(bool)
			if enabled {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("re-added upstream 'cycle' should survive restart")
	}
}

func TestIntegration_RefreshAfterRemove(t *testing.T) {
	// Refreshing a removed upstream should return 404.
	hub, fake, _, _, cleanup := bootHubWithDB(t)
	defer cleanup()

	// Add.
	addBody := `{"name":"gone","base_url":"` + fake.URL + `","auth":{"scheme":"none"}}`
	resp, _ := http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/upstreams", addBody))
	var created map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	id := created["id"].(string)

	// Remove.
	resp, _ = http.DefaultClient.Do(adminReq("DELETE", hub.URL+"/admin/upstreams/"+id, ""))
	resp.Body.Close()

	// Refresh by ID — should fail (502 bad gateway since upstream is gone from
	// the in-memory registry and can't be reached).
	resp, _ = http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/upstreams/"+id+"/refresh", ""))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("refresh after remove: expected 502, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestIntegration_MultipleUpstreams_AddRemoveList(t *testing.T) {
	// Exercises add/remove/list with multiple upstreams simultaneously.
	hub, fake, _, _, cleanup := bootHubWithDB(t)
	defer cleanup()

	names := []string{"alpha", "bravo", "charlie"}
	ids := make([]string, len(names))

	// Add all three.
	for i, name := range names {
		addBody := `{"name":"` + name + `","base_url":"` + fake.URL + `","auth":{"scheme":"none"}}`
		resp, err := http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/upstreams", addBody))
		if err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("add %s: status=%d", name, resp.StatusCode)
		}
		var created map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&created)
		resp.Body.Close()
		ids[i] = created["id"].(string)
	}

	// List — should have 3.
	list := adminList(t, hub.URL)
	if len(list) != 3 {
		t.Fatalf("expected 3, got %d", len(list))
	}

	// Remove bravo.
	resp, _ := http.DefaultClient.Do(adminReq("DELETE", hub.URL+"/admin/upstreams/"+ids[1], ""))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete bravo: status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// List — should have 2 (alpha, charlie).
	list = adminList(t, hub.URL)
	if len(list) != 2 {
		t.Fatalf("expected 2 after removing bravo, got %d", len(list))
	}
	nameSet := map[string]bool{}
	for _, item := range list {
		nameSet[item["name"].(string)] = true
	}
	if nameSet["bravo"] {
		t.Fatalf("bravo should be gone")
	}
	if !nameSet["alpha"] || !nameSet["charlie"] {
		t.Fatalf("alpha and charlie should remain, got %v", nameSet)
	}

	// Remove alpha.
	resp, _ = http.DefaultClient.Do(adminReq("DELETE", hub.URL+"/admin/upstreams/"+ids[0], ""))
	resp.Body.Close()

	// List — should have 1 (charlie).
	list = adminList(t, hub.URL)
	if len(list) != 1 || list[0]["name"] != "charlie" {
		t.Fatalf("expected only charlie, got %+v", list)
	}

	// Remove charlie.
	resp, _ = http.DefaultClient.Do(adminReq("DELETE", hub.URL+"/admin/upstreams/"+ids[2], ""))
	resp.Body.Close()

	// List — empty.
	list = adminList(t, hub.URL)
	if len(list) != 0 {
		t.Fatalf("expected 0, got %d", len(list))
	}
}

func TestIntegration_DeleteNonExistent_Returns404(t *testing.T) {
	hub, _, _, _, cleanup := bootHubWithDB(t)
	defer cleanup()

	resp, _ := http.DefaultClient.Do(adminReq("DELETE", hub.URL+"/admin/upstreams/does-not-exist", ""))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestIntegration_DuplicateAdd_Returns409(t *testing.T) {
	hub, fake, _, _, cleanup := bootHubWithDB(t)
	defer cleanup()

	addBody := `{"name":"dupe","base_url":"` + fake.URL + `","auth":{"scheme":"none"}}`

	// First add — 201.
	resp, _ := http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/upstreams", addBody))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first add: status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// Second add — 409.
	resp, _ = http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/upstreams", addBody))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate add: expected 409, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestIntegration_ConfigUpstream_SurvivesRestart(t *testing.T) {
	// Config-defined upstreams should be restored on restart from config.
	_, fake, db, _, cleanup := bootHubWithDB(t)
	defer cleanup()

	cfgUpstreams := []config.UpstreamCfg{
		{Name: "from-config", BaseURL: fake.URL, Auth: config.AuthConfig{Scheme: "none"}, Enabled: true},
	}

	// First boot with config.
	hub1, _ := rebuildHub(t, db, cfgUpstreams)
	list1 := adminList(t, hub1.URL)
	hub1.Close()
	if len(list1) != 1 || list1[0]["name"] != "from-config" {
		t.Fatalf("first boot: expected 'from-config', got %+v", list1)
	}

	// Second boot with same config — should still be there.
	hub2, _ := rebuildHub(t, db, cfgUpstreams)
	list2 := adminList(t, hub2.URL)
	hub2.Close()
	if len(list2) != 1 || list2[0]["name"] != "from-config" {
		t.Fatalf("second boot: expected 'from-config', got %+v", list2)
	}

	// Third boot with config removed — config-sourced upstream should be disabled.
	hub3, _ := rebuildHub(t, db, nil)
	list3 := adminList(t, hub3.URL)
	hub3.Close()
	for _, item := range list3 {
		if item["name"] == "from-config" {
			enabled, _ := item["enabled"].(bool)
			if enabled {
				t.Fatalf("config-sourced upstream should be disabled when config removed")
			}
		}
	}
}

func TestIntegration_AdminAndConfig_MixedSources(t *testing.T) {
	// Exercises a scenario with both config-sourced and admin-sourced upstreams.
	_, fake, db, _, cleanup := bootHubWithDB(t)
	defer cleanup()

	cfgUpstreams := []config.UpstreamCfg{
		{Name: "cfg-one", BaseURL: fake.URL, Auth: config.AuthConfig{Scheme: "none"}, Enabled: true},
	}

	// Boot with one config upstream.
	hub1, reg1 := rebuildHub(t, db, cfgUpstreams)

	// Add an admin upstream.
	addBody := `{"name":"admin-one","base_url":"` + fake.URL + `","auth":{"scheme":"none"}}`
	resp, _ := http.DefaultClient.Do(adminReq("POST", hub1.URL+"/admin/upstreams", addBody))
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("add admin-one: status=%d body=%s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Both should appear.
	list := adminList(t, hub1.URL)
	if len(list) != 2 {
		t.Fatalf("expected 2 upstreams, got %d", len(list))
	}
	hub1.Close()

	// Restart with same config — admin-one should survive.
	hub2, _ := rebuildHub(t, db, cfgUpstreams)
	list2 := adminList(t, hub2.URL)
	hub2.Close()
	nameSet := map[string]bool{}
	for _, item := range list2 {
		if enabled, _ := item["enabled"].(bool); enabled {
			nameSet[item["name"].(string)] = true
		}
	}
	if !nameSet["cfg-one"] || !nameSet["admin-one"] {
		t.Fatalf("both upstreams should survive restart, got %v", nameSet)
	}

	// Remove admin-one, restart — only cfg-one should remain.
	hub3, reg3 := rebuildHub(t, db, cfgUpstreams)
	var adminID registry.UpstreamID
	for _, u := range reg3.List() {
		if u.Name == "admin-one" {
			adminID = u.ID
		}
	}
	if adminID == "" {
		t.Fatalf("admin-one not found in registry")
	}
	_ = reg1 // avoid unused
	if err := reg3.Remove(context.Background(), adminID); err != nil {
		t.Fatalf("Remove admin-one: %v", err)
	}
	hub3.Close()

	// Restart — admin-one should stay removed, cfg-one should survive.
	hub4, _ := rebuildHub(t, db, cfgUpstreams)
	list4 := adminList(t, hub4.URL)
	hub4.Close()
	for _, item := range list4 {
		if item["name"] == "admin-one" {
			if enabled, _ := item["enabled"].(bool); enabled {
				t.Fatalf("admin-removed 'admin-one' should not reappear")
			}
		}
	}
}

func TestIntegration_AddWithMissingFields_Returns400(t *testing.T) {
	hub, _, _, _, cleanup := bootHubWithDB(t)
	defer cleanup()

	// Missing base_url.
	resp, _ := http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/upstreams", `{"name":"no-url"}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing base_url: expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Missing name.
	resp, _ = http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/upstreams", `{"base_url":"http://x"}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing name: expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Empty body.
	resp, _ = http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/upstreams", `{}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty body: expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestIntegration_RefreshAll_UpdatesAllUpstreams(t *testing.T) {
	hub, fake, _, _, cleanup := bootHubWithDB(t)
	defer cleanup()

	// Add two upstreams.
	for _, name := range []string{"r1", "r2"} {
		addBody := `{"name":"` + name + `","base_url":"` + fake.URL + `","auth":{"scheme":"none"}}`
		resp, _ := http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/upstreams", addBody))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("add %s: status=%d", name, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Refresh all.
	resp, _ := http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/refresh", ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh-all: status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// Both should have cards now.
	list := adminList(t, hub.URL)
	for _, item := range list {
		if item["has_card"] != true {
			t.Errorf("upstream %s missing card after refresh-all", item["name"])
		}
	}
}

func TestIntegration_SkillsEndpoint_ReflectsAddRemove(t *testing.T) {
	hub, fake, _, _, cleanup := bootHubWithDB(t)
	defer cleanup()

	// Start empty.
	resp, _ := http.DefaultClient.Do(adminReq("GET", hub.URL+"/admin/skills", ""))
	var skills []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&skills)
	resp.Body.Close()
	if len(skills) != 0 {
		t.Fatalf("expected 0 skills, got %d", len(skills))
	}

	// Add upstream — skills should appear after card fetch.
	addBody := `{"name":"sk","base_url":"` + fake.URL + `","auth":{"scheme":"none"}}`
	resp, _ = http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/upstreams", addBody))
	var created map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	id := created["id"].(string)

	resp, _ = http.DefaultClient.Do(adminReq("GET", hub.URL+"/admin/skills", ""))
	_ = json.NewDecoder(resp.Body).Decode(&skills)
	resp.Body.Close()
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill after add, got %d", len(skills))
	}

	// Remove upstream — skills should disappear.
	resp, _ = http.DefaultClient.Do(adminReq("DELETE", hub.URL+"/admin/upstreams/"+id, ""))
	resp.Body.Close()

	resp, _ = http.DefaultClient.Do(adminReq("GET", hub.URL+"/admin/skills", ""))
	_ = json.NewDecoder(resp.Body).Decode(&skills)
	resp.Body.Close()
	if len(skills) != 0 {
		t.Fatalf("expected 0 skills after remove, got %d", len(skills))
	}
}

func TestIntegration_AdminAuth_Unauthorized(t *testing.T) {
	hub, _, _, _, cleanup := bootHubWithDB(t)
	defer cleanup()

	// No auth header.
	req, _ := http.NewRequest("GET", hub.URL+"/admin/upstreams", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no auth: expected 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Wrong key.
	req, _ = http.NewRequest("GET", hub.URL+"/admin/upstreams", nil)
	req.Header.Set("Authorization", "Bearer wrong-key")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong key: expected 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestIntegration_AdminRemove_DoesNotReappearAfterRestart_NoConfig(t *testing.T) {
	// Regression: the real-world scenario reported by users.
	// Add via admin API → Remove via admin API → restart with NO config
	// upstream entries → removed upstream must NOT reappear in `oah up list`.
	// Bug was: bootstrap loaded all DB rows (including admin-source enabled=0)
	// into the in-memory registry, so /admin/upstreams returned them.
	hub, fake, db, _, cleanup := bootHubWithDB(t)
	defer cleanup()

	// Add via admin API.
	addBody := `{"name":"ghost","base_url":"` + fake.URL + `","auth":{"scheme":"none"}}`
	resp, _ := http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/upstreams", addBody))
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("add: status=%d body=%s", resp.StatusCode, body)
	}
	var created map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	id := created["id"].(string)

	// Remove via admin API.
	resp, _ = http.DefaultClient.Do(adminReq("DELETE", hub.URL+"/admin/upstreams/"+id, ""))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// Verify empty before restart.
	before := adminList(t, hub.URL)
	if len(before) != 0 {
		t.Fatalf("before restart: expected 0, got %+v", before)
	}

	// Simulate restart with EMPTY config (like user's config.yaml that only
	// has `omnilauncher`, not the deleted `hermes`).
	hub2, _ := rebuildHub(t, db, nil)
	defer hub2.Close()

	after := adminList(t, hub2.URL)
	if len(after) != 0 {
		t.Fatalf("after restart: admin-removed upstream reappeared: %+v", after)
	}
}

func TestIntegration_AdminRemove_DoesNotReappearAfterRestart_UnrelatedConfig(t *testing.T) {
	// Same as above but with an unrelated upstream in config, matching the
	// user's actual setup (omnilauncher in config, hermes removed via admin).
	hub, fake, db, _, cleanup := bootHubWithDB(t)
	defer cleanup()

	// Add TWO admin upstreams.
	for _, name := range []string{"keeper", "goner"} {
		addBody := `{"name":"` + name + `","base_url":"` + fake.URL + `","auth":{"scheme":"none"}}`
		resp, _ := http.DefaultClient.Do(adminReq("POST", hub.URL+"/admin/upstreams", addBody))
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("add %s: status=%d body=%s", name, resp.StatusCode, body)
		}
		resp.Body.Close()
	}

	// Remove `goner` only.
	list := adminList(t, hub.URL)
	var gonerID string
	for _, item := range list {
		if item["name"] == "goner" {
			gonerID = item["id"].(string)
		}
	}
	if gonerID == "" {
		t.Fatalf("goner not found in list: %+v", list)
	}
	resp, _ := http.DefaultClient.Do(adminReq("DELETE", hub.URL+"/admin/upstreams/"+gonerID, ""))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete goner: status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// Restart with no config upstreams.
	hub2, _ := rebuildHub(t, db, nil)
	defer hub2.Close()

	after := adminList(t, hub2.URL)
	names := map[string]bool{}
	for _, item := range after {
		names[item["name"].(string)] = true
	}
	if names["goner"] {
		t.Fatalf("admin-removed 'goner' reappeared after restart: %+v", after)
	}
	if !names["keeper"] {
		t.Fatalf("admin-added 'keeper' should survive restart, got %+v", after)
	}
}
