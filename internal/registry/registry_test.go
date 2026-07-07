package registry

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
	"github.com/OmniLLM/omni-agent-hub/internal/config"
	"github.com/OmniLLM/omni-agent-hub/internal/store"
)

// fakeFetcher lets tests control card-fetch results and count calls.
type fakeFetcher struct {
	card *a2a.AgentCard
	err  error
	n    int
}

func (f *fakeFetcher) Fetch(ctx context.Context, baseURL, scheme, token string) (*a2a.AgentCard, error) {
	f.n++
	if f.err != nil {
		return nil, f.err
	}
	return f.card, nil
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestBootstrap_MergesConfigWithDB(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "hermes", URL: "http://x"}}
	r := New(db, f)

	cfg := []config.UpstreamCfg{
		{Name: "hermes", BaseURL: "http://h", Auth: config.AuthConfig{Scheme: "bearer", Token: "t"}, Enabled: true},
		{Name: "research", BaseURL: "http://r", Auth: config.AuthConfig{Scheme: "none"}, Enabled: true},
	}
	if err := Bootstrap(ctx, r, db, cfg); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	list := r.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 upstreams, got %d", len(list))
	}
	// Re-bootstrap with the same cfg → should be idempotent.
	if err := Bootstrap(ctx, r, db, cfg); err != nil {
		t.Fatalf("re-Bootstrap: %v", err)
	}
	if len(r.List()) != 2 {
		t.Fatalf("re-bootstrap changed count: %d", len(r.List()))
	}
}

func TestAdd_DuplicateNameRejected(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "x", URL: "http://x"}}
	r := New(db, f)

	if _, err := r.Add(ctx, AddInput{Name: "a", BaseURL: "http://a"}); err != nil {
		t.Fatalf("Add first: %v", err)
	}
	if _, err := r.Add(ctx, AddInput{Name: "a", BaseURL: "http://a2"}); !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("expected ErrDuplicateName, got %v", err)
	}
}

func TestBreaker_ThreeFailuresFlipsUnhealthy(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "x", URL: "http://x"}}
	r := New(db, f)
	u, err := r.Add(ctx, AddInput{Name: "u1", BaseURL: "http://u"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Drain the initial add/card events so we can assert the health event later.
	drain(r.Events())

	for i := 0; i < 2; i++ {
		r.RecordFailure(u.ID, errors.New("boom"))
	}
	got, _ := r.Get(u.ID)
	if got.Status != store.StatusHealthy && got.Status != store.StatusUnknown {
		t.Fatalf("after 2 failures should still be healthy/unknown, got %s", got.Status)
	}
	r.RecordFailure(u.ID, errors.New("boom"))
	got, _ = r.Get(u.ID)
	if got.Status != store.StatusUnhealthy {
		t.Fatalf("after 3 failures should be unhealthy, got %s", got.Status)
	}

	// Success flips back and emits a health event.
	r.RecordSuccess(u.ID)
	got, _ = r.Get(u.ID)
	if got.Status != store.StatusHealthy || got.ConsecutiveFailures != 0 {
		t.Fatalf("after success: status=%s failures=%d", got.Status, got.ConsecutiveFailures)
	}
}

func TestCanAttempt_Backoff(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	r := New(db, &fakeFetcher{card: &a2a.AgentCard{Name: "x", URL: "http://x"}})
	u, _ := r.Add(ctx, AddInput{Name: "u", BaseURL: "http://u"})

	// Healthy always attempts.
	if !r.CanAttempt(u.ID) {
		t.Fatalf("healthy upstream should attempt")
	}
	// Force unhealthy state.
	for i := 0; i < 3; i++ {
		r.RecordFailure(u.ID, errors.New("x"))
	}
	if r.CanAttempt(u.ID) {
		t.Fatalf("just-failed upstream should not attempt within backoff window")
	}
	// Manually rewind LastFailureAt to 5s ago (backoff at 3 failures = 2^0=1s).
	impl := r.(*registryImpl)
	impl.mu.Lock()
	impl.byID[u.ID].LastFailureAt = time.Now().Add(-5 * time.Second)
	impl.mu.Unlock()
	if !r.CanAttempt(u.ID) {
		t.Fatalf("after backoff window elapsed, should attempt again")
	}
}

func TestRefreshCard_InvalidCardRejected(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "", URL: ""}}
	r := New(db, f)
	u, _ := r.Add(ctx, AddInput{Name: "u", BaseURL: "http://u"})
	err := r.RefreshCard(ctx, u.ID)
	if err == nil {
		t.Fatalf("expected invalid card error")
	}
}

func TestRefreshCard_FetchFailureClearsStaleCard(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "fake", URL: "http://u"}}
	r := New(db, f)
	u, err := r.Add(ctx, AddInput{Name: "u", BaseURL: "http://u"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, ok := r.Get(u.ID)
	if !ok {
		t.Fatalf("Get after add: not found")
	}
	if got.Card == nil {
		t.Fatalf("expected cached card after successful add refresh")
	}
	if got.Status != store.StatusHealthy {
		t.Fatalf("expected healthy after initial fetch, got %s", got.Status)
	}

	drain(r.Events())
	f.err = errors.New("upstream down")
	if err := r.RefreshCard(ctx, u.ID); err == nil {
		t.Fatalf("expected refresh error")
	}
	got, ok = r.Get(u.ID)
	if !ok {
		t.Fatalf("Get after refresh failure: not found")
	}
	if got.Card != nil {
		t.Fatalf("expected stale card to be cleared after refresh failure")
	}
	if got.Status != store.StatusUnknown {
		t.Fatalf("expected status unknown after stale-card eviction, got %s", got.Status)
	}

	select {
	case ev := <-r.Events():
		if ev.Kind != EventCardChanged {
			t.Fatalf("expected EventCardChanged, got %v", ev.Kind)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("expected card-changed event after stale-card eviction")
	}
}

// drain reads any pending events on ch until it blocks briefly.
func drain(ch <-chan Event) {
	for {
		select {
		case <-ch:
		case <-time.After(10 * time.Millisecond):
			return
		}
	}
}

func TestRefreshCard_UnhealthyBeforeBackoff_StaysUnhealthy(t *testing.T) {
	// A successful card fetch during the backoff window (just after 3
	// failures) must NOT resurrect the upstream — that would flap on
	// upstreams that come and go every few seconds.
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "u", URL: "http://u"}}
	r := New(db, f)
	u, err := r.Add(ctx, AddInput{Name: "u", BaseURL: "http://u"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Trip the breaker: 3 failures → unhealthy with backoff = 1s.
	for i := 0; i < 3; i++ {
		r.RecordFailure(u.ID, errors.New("boom"))
	}
	got, _ := r.Get(u.ID)
	if got.Status != store.StatusUnhealthy {
		t.Fatalf("expected unhealthy after 3 failures, got %s", got.Status)
	}
	// LastFailureAt is "now", so the 1s window has not elapsed.
	if err := r.RefreshCard(ctx, u.ID); err != nil {
		t.Fatalf("RefreshCard: %v", err)
	}
	got, _ = r.Get(u.ID)
	if got.Status != store.StatusUnhealthy {
		t.Fatalf("unhealthy upstream healed too eagerly during backoff: %s", got.Status)
	}
}

func TestRefreshCard_UnhealthyAfterBackoff_HealsAndResetsFailures(t *testing.T) {
	// After the backoff window elapses, a successful card fetch should
	// restore healthy status and clear the failure counter, breaking the
	// composite-card discovery deadlock.
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "u", URL: "http://u"}}
	r := New(db, f)
	u, err := r.Add(ctx, AddInput{Name: "u", BaseURL: "http://u"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	for i := 0; i < 3; i++ {
		r.RecordFailure(u.ID, errors.New("boom"))
	}
	// Rewind LastFailureAt so the 1s backoff window has definitely elapsed.
	impl := r.(*registryImpl)
	impl.mu.Lock()
	impl.byID[u.ID].LastFailureAt = time.Now().Add(-5 * time.Second)
	impl.mu.Unlock()

	if err := r.RefreshCard(ctx, u.ID); err != nil {
		t.Fatalf("RefreshCard: %v", err)
	}
	got, _ := r.Get(u.ID)
	if got.Status != store.StatusHealthy {
		t.Fatalf("expected healthy after post-backoff refresh, got %s", got.Status)
	}
	if got.ConsecutiveFailures != 0 {
		t.Fatalf("expected ConsecutiveFailures reset to 0, got %d", got.ConsecutiveFailures)
	}
}

func TestAdd_ReAddAfterRemove_ReusesDBID(t *testing.T) {
	// Regression: Remove soft-deletes (enabled=false) in DB but removes from
	// memory. A subsequent Add generates a new UUID that wins in-memory but
	// loses to UpsertUpstream's ON CONFLICT(name) in the DB, leaving the DB
	// row with the old id. Dispatch then fails with a FK constraint error
	// because CreateTask references the in-memory id that doesn't exist in
	// the upstreams table.
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "u", URL: "http://u"}}
	r := New(db, f)

	// Step 1: Add an upstream.
	original, err := r.Add(ctx, AddInput{Name: "u", BaseURL: "http://u"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	origID := original.ID

	// Step 2: Remove it (soft-delete in DB, gone from memory).
	if err := r.Remove(ctx, origID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := r.Get(origID); ok {
		t.Fatalf("expected upstream to be gone from memory after Remove")
	}
	// DB row should still exist (disabled).
	row, err := db.GetUpstreamByName(ctx, "u")
	if err != nil {
		t.Fatalf("DB row should still exist after soft-delete: %v", err)
	}
	if row.ID != string(origID) {
		t.Fatalf("DB row id mismatch: got %s, want %s", row.ID, origID)
	}

	// Step 3: Re-add the same name.
	drain(r.Events())
	readded, err := r.Add(ctx, AddInput{Name: "u", BaseURL: "http://u2"})
	if err != nil {
		t.Fatalf("re-Add: %v", err)
	}
	// The in-memory id must match the DB id (the original one).
	if readded.ID != origID {
		t.Fatalf("re-Add id mismatch: in-memory=%s, want=%s (original DB id)", readded.ID, origID)
	}
	// Verify via Get as well.
	got, ok := r.Get(origID)
	if !ok {
		t.Fatalf("Get(origID) should find the re-added upstream")
	}
	if got.BaseURL != "http://u2" {
		t.Fatalf("expected updated base_url, got %s", got.BaseURL)
	}
	// And the DB row should also match.
	row2, err := db.GetUpstreamByName(ctx, "u")
	if err != nil {
		t.Fatalf("DB lookup after re-add: %v", err)
	}
	if row2.ID != string(origID) {
		t.Fatalf("DB id should still be the original: got %s, want %s", row2.ID, origID)
	}
	if row2.BaseURL != "http://u2" {
		t.Fatalf("DB base_url should be updated: got %s", row2.BaseURL)
	}

	// Step 4: Verify CreateTask would work (simulate FK check).
	if err := db.CreateTask(ctx, "test-task-1", "ctx-1", string(origID)); err != nil {
		t.Fatalf("CreateTask with persisted id should succeed: %v", err)
	}
}

func TestBootstrap_SkipsAdminDisabledRows(t *testing.T) {
	// Regression: bootstrap loaded ALL DB rows (including admin-source
	// enabled=0) into the in-memory registry. That made admin-removed
	// upstreams reappear in `oah up list` after restart.
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "u", URL: "http://u"}}
	r := New(db, f)

	// Add + remove an admin upstream (soft-deletes in DB).
	u, err := r.Add(ctx, AddInput{Name: "removed", BaseURL: "http://x"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := r.Remove(ctx, u.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// The DB row still exists (soft-delete for FK integrity).
	row, err := db.GetUpstreamByName(ctx, "removed")
	if err != nil {
		t.Fatalf("DB row missing after soft-delete: %v", err)
	}
	if row.Enabled {
		t.Fatalf("DB row should be disabled after Remove")
	}

	// Bootstrap a fresh registry from the same DB with no config.
	r2 := New(db, f)
	if err := Bootstrap(ctx, r2, db, nil); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// The admin-disabled row must NOT be loaded into memory.
	if len(r2.List()) != 0 {
		t.Fatalf("admin-disabled row should not be loaded, got %+v", r2.List())
	}
	if _, ok := r2.GetByName("removed"); ok {
		t.Fatalf("GetByName should not find admin-disabled row")
	}
}

func TestBootstrap_KeepsAdminEnabledRows(t *testing.T) {
	// Verify that admin-source enabled rows ARE loaded (regression guard).
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "u", URL: "http://u"}}
	r := New(db, f)

	// Add an admin upstream (not removed).
	if _, err := r.Add(ctx, AddInput{Name: "kept", BaseURL: "http://x"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Bootstrap fresh registry.
	r2 := New(db, f)
	if err := Bootstrap(ctx, r2, db, nil); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if len(r2.List()) != 1 {
		t.Fatalf("admin-enabled row should be loaded, got %+v", r2.List())
	}
}

func TestBootstrap_DoesNotReEnableAdminRemovedUpstream(t *testing.T) {
	// An admin-removed upstream that is NOT listed in config must stay
	// removed after restart. (If config still lists it, config wins — see
	// TestBootstrap_ConfigOverridesAdminRemoval.)
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "x", URL: "http://x"}}
	r := New(db, f)

	// Step 1: Bootstrap with one config-defined upstream.
	cfg := []config.UpstreamCfg{
		{Name: "alpha", BaseURL: "http://alpha", Auth: config.AuthConfig{Scheme: "none"}, Enabled: true},
	}
	if err := Bootstrap(ctx, r, db, cfg); err != nil {
		t.Fatalf("Bootstrap 1: %v", err)
	}
	if len(r.List()) != 1 {
		t.Fatalf("expected 1 upstream, got %d", len(r.List()))
	}

	// Step 2: Admin-add an upstream manually (simulates `oah up add`).
	admin, err := r.Add(ctx, AddInput{Name: "beta", BaseURL: "http://beta"})
	if err != nil {
		t.Fatalf("Add admin upstream: %v", err)
	}
	if len(r.List()) != 2 {
		t.Fatalf("expected 2 upstreams, got %d", len(r.List()))
	}

	// Step 3: Remove the admin upstream (simulates `oah up remove`).
	if err := r.Remove(ctx, admin.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(r.List()) != 1 {
		t.Fatalf("expected 1 upstream after remove, got %d", len(r.List()))
	}

	// Step 4: Re-bootstrap with config that does NOT include the removed
	// upstream (matching real-world case: hermes was admin-added and only
	// omnilauncher is in config.yaml).
	r2 := New(db, f)
	if err := Bootstrap(ctx, r2, db, cfg); err != nil {
		t.Fatalf("Bootstrap 2: %v", err)
	}

	// The admin-removed upstream must NOT reappear.
	for _, u := range r2.List() {
		if u.Name == "beta" {
			t.Fatalf("admin-removed upstream 'beta' reappeared after restart — bug!")
		}
	}
}

func TestBootstrap_ConfigOverridesAdminRemoval(t *testing.T) {
	// If a user explicitly lists an upstream in config.yaml, config wins:
	// the upstream is (re-)created even if admin previously removed it.
	// This is the declarative-config-is-source-of-truth interpretation.
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "x", URL: "http://x"}}
	r := New(db, f)

	// Add + remove via admin.
	u, err := r.Add(ctx, AddInput{Name: "beta", BaseURL: "http://beta"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := r.Remove(ctx, u.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Restart with config that explicitly lists it.
	r2 := New(db, f)
	cfg := []config.UpstreamCfg{
		{Name: "beta", BaseURL: "http://beta", Auth: config.AuthConfig{Scheme: "none"}, Enabled: true},
	}
	if err := Bootstrap(ctx, r2, db, cfg); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// The upstream should be present and enabled because config re-declares it.
	found := false
	for _, u := range r2.List() {
		if u.Name == "beta" && u.Enabled {
			found = true
		}
	}
	if !found {
		t.Fatalf("config-declared upstream 'beta' should be present after restart")
	}
}

func TestBootstrap_ConfigDisabledUpstreamStaysDisabled(t *testing.T) {
	// Verify that a config entry with enabled=false does get disabled in DB.
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "x", URL: "http://x"}}
	r := New(db, f)

	cfg := []config.UpstreamCfg{
		{Name: "gamma", BaseURL: "http://g", Auth: config.AuthConfig{Scheme: "none"}, Enabled: true},
	}
	if err := Bootstrap(ctx, r, db, cfg); err != nil {
		t.Fatalf("Bootstrap 1: %v", err)
	}
	if len(r.List()) != 1 {
		t.Fatalf("expected 1 upstream, got %d", len(r.List()))
	}

	// Re-bootstrap with enabled=false in config.
	r2 := New(db, f)
	cfg2 := []config.UpstreamCfg{
		{Name: "gamma", BaseURL: "http://g", Auth: config.AuthConfig{Scheme: "none"}, Enabled: false},
	}
	if err := Bootstrap(ctx, r2, db, cfg2); err != nil {
		t.Fatalf("Bootstrap 2: %v", err)
	}

	// Config-sourced upstreams can be disabled by config.
	for _, u := range r2.List() {
		if u.Name == "gamma" && u.Enabled {
			t.Fatalf("config-disabled upstream 'gamma' should not be enabled")
		}
	}
}

func TestBootstrap_ConfigReEnablesConfigSourced(t *testing.T) {
	// Config-sourced (not admin) upstreams that were disabled should be
	// re-enabled when config says enabled=true.
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "x", URL: "http://x"}}

	// Bootstrap with enabled=true → then enabled=false → then enabled=true.
	r1 := New(db, f)
	cfg := []config.UpstreamCfg{
		{Name: "delta", BaseURL: "http://d", Auth: config.AuthConfig{Scheme: "none"}, Enabled: true},
	}
	if err := Bootstrap(ctx, r1, db, cfg); err != nil {
		t.Fatalf("Bootstrap 1: %v", err)
	}

	// Disable via config.
	r2 := New(db, f)
	cfg[0].Enabled = false
	if err := Bootstrap(ctx, r2, db, cfg); err != nil {
		t.Fatalf("Bootstrap 2: %v", err)
	}

	// Re-enable via config — should work because source is config, not admin.
	r3 := New(db, f)
	cfg[0].Enabled = true
	if err := Bootstrap(ctx, r3, db, cfg); err != nil {
		t.Fatalf("Bootstrap 3: %v", err)
	}
	found := false
	for _, u := range r3.List() {
		if u.Name == "delta" && u.Enabled {
			found = true
		}
	}
	if !found {
		t.Fatalf("config-sourced upstream 'delta' should have been re-enabled by config")
	}
}

func TestRemove_ListDoesNotShowRemoved(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "u", URL: "http://u"}}
	r := New(db, f)

	u1, _ := r.Add(ctx, AddInput{Name: "a", BaseURL: "http://a"})
	u2, _ := r.Add(ctx, AddInput{Name: "b", BaseURL: "http://b"})

	if len(r.List()) != 2 {
		t.Fatalf("expected 2, got %d", len(r.List()))
	}

	if err := r.Remove(ctx, u1.ID); err != nil {
		t.Fatalf("Remove u1: %v", err)
	}
	list := r.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 after remove, got %d", len(list))
	}
	if list[0].Name != "b" {
		t.Fatalf("expected 'b' remaining, got %q", list[0].Name)
	}

	// Remove the other one.
	if err := r.Remove(ctx, u2.ID); err != nil {
		t.Fatalf("Remove u2: %v", err)
	}
	if len(r.List()) != 0 {
		t.Fatalf("expected 0 after removing all, got %d", len(r.List()))
	}
}

func TestRemove_GetReturnsFalse(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "u", URL: "http://u"}}
	r := New(db, f)

	u, _ := r.Add(ctx, AddInput{Name: "a", BaseURL: "http://a"})
	if err := r.Remove(ctx, u.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := r.Get(u.ID); ok {
		t.Fatalf("Get should return false after Remove")
	}
	if _, ok := r.GetByName("a"); ok {
		t.Fatalf("GetByName should return false after Remove")
	}
}

func TestRemove_NonExistent_ReturnsErrNotFound(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	r := New(db, &fakeFetcher{card: &a2a.AgentCard{Name: "x", URL: "http://x"}})

	err := r.Remove(ctx, "nonexistent-id")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestRemove_EmitsEvent(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "u", URL: "http://u"}}
	r := New(db, f)

	u, _ := r.Add(ctx, AddInput{Name: "ev", BaseURL: "http://ev"})
	drain(r.Events())

	if err := r.Remove(ctx, u.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	select {
	case ev := <-r.Events():
		if ev.Kind != EventRemoved {
			t.Fatalf("expected EventRemoved, got %v", ev.Kind)
		}
		if ev.ID != u.ID {
			t.Fatalf("event ID = %s, want %s", ev.ID, u.ID)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("expected EventRemoved but timed out")
	}
}

func TestRefreshCard_UnknownBecomesHealthy_Preserved(t *testing.T) {
	// Regression guard: the unknown → healthy transition on a successful
	// first fetch must survive the RefreshCard rewrite.
	ctx := context.Background()
	db := openTestStore(t)
	f := &fakeFetcher{card: &a2a.AgentCard{Name: "u", URL: "http://u"}}
	r := New(db, f)
	u, err := r.Add(ctx, AddInput{Name: "u", BaseURL: "http://u"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Add triggers an initial fetch that flips unknown → healthy.
	got, _ := r.Get(u.ID)
	if got.Status != store.StatusHealthy {
		t.Fatalf("expected healthy after initial fetch, got %s", got.Status)
	}
}
