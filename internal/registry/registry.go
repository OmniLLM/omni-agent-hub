// Package registry owns the authoritative in-memory upstream list, agent-card
// cache, and health state. All mutations emit change events on a buffered
// channel that the card builder subscribes to.
//
// Concurrency model:
//   - The public list/lookup methods acquire an RWMutex read lock.
//   - Mutation methods take the write lock, then send a non-blocking event on
//     the Events() channel (capacity 1; dropped if the previous event has not
//     been consumed — the consumer will pick up the latest state anyway).
package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
	"github.com/OmniLLM/omni-agent-hub/internal/config"
	"github.com/OmniLLM/omni-agent-hub/internal/store"
)

// UpstreamID is the stable id of an upstream (persistent across restarts).
type UpstreamID string

// HealthStatus mirrors store.HealthStatus for callers who don't need the store.
type HealthStatus = store.HealthStatus

// Source mirrors store.UpstreamSource.
type Source = store.UpstreamSource

// EventKind categorizes registry changes.
type EventKind int

const (
	EventAdded EventKind = iota
	EventRemoved
	EventUpdated
	EventHealthChanged
	EventCardChanged
)

func (k EventKind) String() string {
	switch k {
	case EventAdded:
		return "added"
	case EventRemoved:
		return "removed"
	case EventUpdated:
		return "updated"
	case EventHealthChanged:
		return "health"
	case EventCardChanged:
		return "card"
	}
	return "unknown"
}

// Event describes a registry change.
type Event struct {
	Kind EventKind
	ID   UpstreamID
}

// AuthConfig mirrors config.AuthConfig for external callers.
type AuthConfig = config.AuthConfig

// Upstream is the in-memory representation of a registered upstream agent.
type Upstream struct {
	ID                  UpstreamID
	Name                string
	BaseURL             string
	Auth                AuthConfig
	Prefix              string
	Enabled             bool
	Source              Source
	Status              HealthStatus
	Card                *a2a.AgentCard
	ConsecutiveFailures int
	LastSuccessAt       time.Time
	LastFailureAt       time.Time
	CardFetchedAt       time.Time
}

// AddInput carries the parameters for adding an upstream via the admin API.
type AddInput struct {
	Name    string
	BaseURL string
	Auth    AuthConfig
	Prefix  string
}

// Registry is the interface the rest of the hub sees.
type Registry interface {
	List() []Upstream
	Get(id UpstreamID) (Upstream, bool)
	GetByName(name string) (Upstream, bool)

	Add(ctx context.Context, in AddInput) (Upstream, error)
	Remove(ctx context.Context, id UpstreamID) error
	RefreshCard(ctx context.Context, id UpstreamID) error
	RefreshAll(ctx context.Context) error

	RecordSuccess(id UpstreamID)
	RecordFailure(id UpstreamID, err error)

	CanAttempt(id UpstreamID) bool

	Events() <-chan Event
}

// ErrNotFound is returned when the requested upstream id/name is absent.
var ErrNotFound = errors.New("registry: upstream not found")

// ErrDuplicateName is returned when Add would overwrite an existing name.
var ErrDuplicateName = errors.New("registry: duplicate upstream name")

// Fetcher fetches a well-known agent card. Split into an interface so tests can
// swap it out.
type Fetcher interface {
	Fetch(ctx context.Context, baseURL, authScheme, token string) (*a2a.AgentCard, error)
}

// registryImpl is the concrete Registry implementation.
type registryImpl struct {
	mu      sync.RWMutex
	byID    map[UpstreamID]*Upstream
	byName  map[string]UpstreamID
	events  chan Event
	store   *store.Store
	fetcher Fetcher
}

// New constructs a Registry. `db` is required; `fetcher` may be nil to use the
// default HTTP fetcher.
func New(db *store.Store, fetcher Fetcher) Registry {
	if fetcher == nil {
		fetcher = &HTTPFetcher{Client: &http.Client{Timeout: 5 * time.Second}}
	}
	return &registryImpl{
		byID:    make(map[UpstreamID]*Upstream),
		byName:  make(map[string]UpstreamID),
		events:  make(chan Event, 1),
		store:   db,
		fetcher: fetcher,
	}
}

// Bootstrap loads all upstream rows from the store, then upserts every entry
// from cfg with source=config. Any config entries missing from the DB get new
// UUIDs. Entries that used to be in config but no longer are get enabled=0
// (soft-disabled, retained for history).
func Bootstrap(ctx context.Context, r Registry, db *store.Store, cfg []config.UpstreamCfg) error {
	impl, ok := r.(*registryImpl)
	if !ok {
		return errors.New("registry.Bootstrap: unexpected implementation")
	}
	return impl.bootstrap(ctx, cfg)
}

func (r *registryImpl) bootstrap(ctx context.Context, cfg []config.UpstreamCfg) error {
	rows, err := r.store.ListUpstreams(ctx, false)
	if err != nil {
		return fmt.Errorf("registry: loading upstreams from store: %w", err)
	}

	// Config entries win on `base_url` / `auth`; DB retains health state.
	cfgByName := make(map[string]config.UpstreamCfg, len(cfg))
	for _, c := range cfg {
		cfgByName[c.Name] = c
	}

	// 1) Take DB rows as the starting point.
	//    Skip admin-source rows that were soft-deleted via `oah up remove`
	//    UNLESS config re-declares them (in which case config wins and the
	//    upstream is re-adopted with the original DB id preserved).
	//    Rationale: admin-removed rows are retained in DB only to keep
	//    tasks.upstream_id FK references valid; they must not resurface in
	//    the in-memory registry after restart.
	for _, row := range rows {
		if row.Source == store.SourceAdmin && !row.Enabled {
			if _, reclaimed := cfgByName[row.Name]; !reclaimed {
				continue
			}
		}
		u := upstreamFromRow(row)
		r.byID[u.ID] = u
		r.byName[u.Name] = u.ID
	}

	// 2) Overlay config entries: create or update BaseURL/Auth/Prefix/Enabled.
	for _, c := range cfg {
		id, ok := r.byName[c.Name]
		if !ok {
			id = UpstreamID(uuid.NewString())
			u := &Upstream{
				ID:      id,
				Name:    c.Name,
				BaseURL: c.BaseURL,
				Auth:    c.Auth,
				Prefix:  c.Prefix,
				Enabled: c.Enabled,
				Source:  store.SourceConfig,
				Status:  store.StatusUnknown,
			}
			r.byID[id] = u
			r.byName[c.Name] = id
		} else {
			u := r.byID[id]
			u.BaseURL = c.BaseURL
			u.Auth = c.Auth
			u.Prefix = c.Prefix
			// Config re-declaration re-enables the upstream (this covers
			// the admin-removed + re-declared-in-config case; step 1
			// already re-adopted the DB row so id/FKs stay stable).
			u.Enabled = c.Enabled
			// Do NOT downgrade source from admin.
		}
		u := r.byID[id]
		if err := r.store.UpsertUpstream(ctx, toRow(u)); err != nil {
			return fmt.Errorf("registry bootstrap: persist %q: %w", u.Name, err)
		}
	}

	// 3) Soft-disable DB entries that vanished from config (source=config only).
	for _, u := range r.byID {
		if u.Source != store.SourceConfig {
			continue
		}
		if _, stillInCfg := cfgByName[u.Name]; !stillInCfg && u.Enabled {
			u.Enabled = false
			if err := r.store.SetUpstreamEnabled(ctx, string(u.ID), false); err != nil {
				return fmt.Errorf("registry bootstrap: disable stale %q: %w", u.Name, err)
			}
		}
	}
	return nil
}

// --- Query ---------------------------------------------------------------

func (r *registryImpl) List() []Upstream {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Upstream, 0, len(r.byID))
	for _, u := range r.byID {
		out = append(out, *u)
	}
	return out
}

func (r *registryImpl) Get(id UpstreamID) (Upstream, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	u, ok := r.byID[id]
	if !ok {
		return Upstream{}, false
	}
	return *u, true
}

func (r *registryImpl) GetByName(name string) (Upstream, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byName[name]
	if !ok {
		return Upstream{}, false
	}
	return *r.byID[id], true
}

// --- Mutation ------------------------------------------------------------

func (r *registryImpl) Add(ctx context.Context, in AddInput) (Upstream, error) {
	if in.Name == "" || in.BaseURL == "" {
		return Upstream{}, errors.New("registry.Add: name and base_url are required")
	}
	r.mu.Lock()
	if _, exists := r.byName[in.Name]; exists {
		r.mu.Unlock()
		return Upstream{}, ErrDuplicateName
	}
	id := UpstreamID(uuid.NewString())
	u := &Upstream{
		ID:      id,
		Name:    in.Name,
		BaseURL: in.BaseURL,
		Auth:    in.Auth,
		Prefix:  in.Prefix,
		Enabled: true,
		Source:  store.SourceAdmin,
		Status:  store.StatusUnknown,
	}
	r.byID[id] = u
	r.byName[in.Name] = id
	row := toRow(u)
	r.mu.Unlock()

	if err := r.store.UpsertUpstream(ctx, row); err != nil {
		return Upstream{}, err
	}

	// UpsertUpstream uses ON CONFLICT(name) which preserves the existing
	// row's primary key. If a soft-deleted row already existed for this name,
	// the DB id will differ from the UUID we just generated. Read back the
	// actual persisted id and reconcile the in-memory maps so that dispatch's
	// CreateTask (which has a FK to upstreams.id) uses the correct id.
	persisted, err := r.store.GetUpstreamByName(ctx, in.Name)
	if err != nil {
		return Upstream{}, fmt.Errorf("registry.Add: read-back after upsert: %w", err)
	}
	persistedID := UpstreamID(persisted.ID)
	if persistedID != id {
		slog.Info("registry.Add: reusing existing DB id for re-added upstream",
			"upstream", in.Name, "new_id", id, "persisted_id", persistedID)
		r.mu.Lock()
		delete(r.byID, id)
		u.ID = persistedID
		r.byID[persistedID] = u
		r.byName[in.Name] = persistedID
		r.mu.Unlock()
		id = persistedID
	}

	r.emit(Event{Kind: EventAdded, ID: id})
	// Kick off a card fetch. Errors are logged but don't fail the Add.
	if err := r.RefreshCard(ctx, id); err != nil {
		slog.Warn("card fetch failed after add", "upstream", in.Name, "err", err)
	}
	return *u, nil
}

func (r *registryImpl) Remove(ctx context.Context, id UpstreamID) error {
	r.mu.RLock()
	u, ok := r.byID[id]
	if !ok {
		r.mu.RUnlock()
		return ErrNotFound
	}
	name := u.Name
	r.mu.RUnlock()

	// Update the DB FIRST so a failure here doesn't leave the in-memory
	// registry inconsistent with what will be loaded on restart.
	if err := r.store.SetUpstreamEnabled(ctx, string(id), false); err != nil {
		return fmt.Errorf("registry.Remove: %w", err)
	}
	r.mu.Lock()
	delete(r.byID, id)
	delete(r.byName, name)
	r.mu.Unlock()
	r.emit(Event{Kind: EventRemoved, ID: id})
	return nil
}

// RefreshCard fetches the well-known card from the upstream and stores it.
// A failed fetch does NOT flip the breaker (breaker cares about real request
// traffic, not card polls); it just leaves the last known card in place and
// logs a warning.
func (r *registryImpl) RefreshCard(ctx context.Context, id UpstreamID) error {
	r.mu.RLock()
	u, ok := r.byID[id]
	if !ok {
		r.mu.RUnlock()
		return ErrNotFound
	}
	// Snapshot fetch parameters under the lock; release before I/O.
	baseURL, scheme, token := u.BaseURL, u.Auth.Scheme, u.Auth.Token
	r.mu.RUnlock()

	card, err := r.fetcher.Fetch(ctx, baseURL, scheme, token)
	if err != nil {
		r.mu.Lock()
		u, ok = r.byID[id]
		if !ok {
			r.mu.Unlock()
			return ErrNotFound
		}
		hadCard := u.Card != nil
		u.Card = nil
		u.CardFetchedAt = time.Time{}
		if u.Status == store.StatusHealthy {
			u.Status = store.StatusUnknown
		}
		row := toRow(u)
		r.mu.Unlock()
		if err2 := r.store.UpdateUpstreamHealth(ctx, row); err2 != nil {
			return fmt.Errorf("persisting stale-card eviction after fetch failure: %w", err2)
		}
		if hadCard {
			r.emit(Event{Kind: EventCardChanged, ID: id})
		}
		return fmt.Errorf("fetching card from %s: %w", baseURL, err)
	}
	if err := validateCard(card); err != nil {
		return fmt.Errorf("invalid card from %s: %w", baseURL, err)
	}

	r.mu.Lock()
	u, ok = r.byID[id]
	if !ok {
		r.mu.Unlock()
		return ErrNotFound
	}
	u.Card = card
	u.CardFetchedAt = time.Now().UTC()
	// A successful card fetch always flips unknown → healthy, and rescues
	// unhealthy once the current exponential-backoff window has elapsed.
	//
	// The old rule (only rescue from `unknown`) caused a discovery deadlock:
	// composite AgentCard hides unhealthy upstreams, so clients couldn't
	// even send the request that would trigger RecordSuccess. Trusting the
	// most recent evidence — a successful card fetch after the backoff —
	// breaks that deadlock while still avoiding rapid flap right after a
	// failure. `ConsecutiveFailures` resets so subsequent breaker math
	// starts fresh.
	switch u.Status {
	case store.StatusUnknown:
		u.Status = store.StatusHealthy
	case store.StatusUnhealthy:
		if time.Since(u.LastFailureAt) >= unhealthyBackoff(u.ConsecutiveFailures) {
			u.Status = store.StatusHealthy
			u.ConsecutiveFailures = 0
		}
	}
	row := toRow(u)
	r.mu.Unlock()

	if err := r.store.UpdateUpstreamHealth(ctx, row); err != nil {
		return fmt.Errorf("persisting card cache: %w", err)
	}
	r.emit(Event{Kind: EventCardChanged, ID: id})
	return nil
}

// RefreshAll refreshes every enabled upstream's card serially.
func (r *registryImpl) RefreshAll(ctx context.Context) error {
	r.mu.RLock()
	ids := make([]UpstreamID, 0, len(r.byID))
	for id, u := range r.byID {
		if u.Enabled {
			ids = append(ids, id)
		}
	}
	r.mu.RUnlock()
	for _, id := range ids {
		if err := r.RefreshCard(ctx, id); err != nil {
			slog.Warn("card refresh failed", "id", id, "err", err)
		}
	}
	return nil
}

// --- Breaker / health -----------------------------------------------------

// RecordSuccess resets the failure counter and flips status back to healthy.
func (r *registryImpl) RecordSuccess(id UpstreamID) {
	r.mu.Lock()
	u, ok := r.byID[id]
	if !ok {
		r.mu.Unlock()
		return
	}
	prev := u.Status
	u.ConsecutiveFailures = 0
	u.Status = store.StatusHealthy
	u.LastSuccessAt = time.Now().UTC()
	row := toRow(u)
	r.mu.Unlock()

	_ = r.store.UpdateUpstreamHealth(context.Background(), row)
	if prev != store.StatusHealthy {
		r.emit(Event{Kind: EventHealthChanged, ID: id})
	}
}

// RecordFailure increments the failure counter and flips to unhealthy at 3+.
func (r *registryImpl) RecordFailure(id UpstreamID, _ error) {
	r.mu.Lock()
	u, ok := r.byID[id]
	if !ok {
		r.mu.Unlock()
		return
	}
	prev := u.Status
	u.ConsecutiveFailures++
	u.LastFailureAt = time.Now().UTC()
	if u.ConsecutiveFailures >= 3 {
		u.Status = store.StatusUnhealthy
	}
	row := toRow(u)
	r.mu.Unlock()

	_ = r.store.UpdateUpstreamHealth(context.Background(), row)
	if prev != store.StatusUnhealthy && u.ConsecutiveFailures >= 3 {
		r.emit(Event{Kind: EventHealthChanged, ID: id})
	}
}

// CanAttempt implements the exponential backoff gate. Returns true if a
// request may be attempted right now. Healthy upstreams always pass.
// Unhealthy ones pass only after the backoff window has elapsed since the
// last failure.
func (r *registryImpl) CanAttempt(id UpstreamID) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	u, ok := r.byID[id]
	if !ok {
		return false
	}
	if u.Status != store.StatusUnhealthy {
		return true
	}
	return time.Since(u.LastFailureAt) >= unhealthyBackoff(u.ConsecutiveFailures)
}

// unhealthyBackoff returns the current exponential-backoff window for an
// upstream with `failures` consecutive failures. The breaker flips unhealthy
// at 3+ failures, and the window doubles each additional failure up to a
// 64-second ceiling: 1s, 2s, 4s, ... 64s.
func unhealthyBackoff(failures int) time.Duration {
	exp := failures - 3
	if exp < 0 {
		exp = 0
	}
	if exp > 6 {
		exp = 6
	}
	return time.Duration(math.Pow(2, float64(exp))) * time.Second
}

// --- Events --------------------------------------------------------------

func (r *registryImpl) Events() <-chan Event { return r.events }

// emit sends a non-blocking event; if the channel already has a pending event,
// the new one is dropped (the consumer will re-read state anyway).
func (r *registryImpl) emit(e Event) {
	select {
	case r.events <- e:
	default:
	}
}

// --- Row <-> Upstream conversion -----------------------------------------

func toRow(u *Upstream) store.UpstreamRow {
	row := store.UpstreamRow{
		ID:                  string(u.ID),
		Name:                u.Name,
		BaseURL:             u.BaseURL,
		AuthScheme:          u.Auth.Scheme,
		AuthToken:           u.Auth.Token,
		Prefix:              u.Prefix,
		Enabled:             u.Enabled,
		Source:              u.Source,
		Status:              u.Status,
		ConsecutiveFailures: u.ConsecutiveFailures,
	}
	if !u.LastSuccessAt.IsZero() {
		row.LastSuccessAt = nullString(u.LastSuccessAt.Format(time.RFC3339Nano))
	}
	if !u.LastFailureAt.IsZero() {
		row.LastFailureAt = nullString(u.LastFailureAt.Format(time.RFC3339Nano))
	}
	if u.Card != nil {
		if b, err := json.Marshal(u.Card); err == nil {
			row.CardJSON = nullString(string(b))
		}
	}
	if !u.CardFetchedAt.IsZero() {
		row.CardFetchedAt = nullString(u.CardFetchedAt.Format(time.RFC3339Nano))
	}
	return row
}

func upstreamFromRow(row store.UpstreamRow) *Upstream {
	u := &Upstream{
		ID:      UpstreamID(row.ID),
		Name:    row.Name,
		BaseURL: row.BaseURL,
		Auth: AuthConfig{
			Scheme: row.AuthScheme,
			Token:  row.AuthToken,
		},
		Prefix:              row.Prefix,
		Enabled:             row.Enabled,
		Source:              row.Source,
		Status:              row.Status,
		ConsecutiveFailures: row.ConsecutiveFailures,
		Card:                row.Card(),
	}
	if row.LastSuccessAt.Valid {
		if t, err := time.Parse(time.RFC3339Nano, row.LastSuccessAt.String); err == nil {
			u.LastSuccessAt = t
		}
	}
	if row.LastFailureAt.Valid {
		if t, err := time.Parse(time.RFC3339Nano, row.LastFailureAt.String); err == nil {
			u.LastFailureAt = t
		}
	}
	if row.CardFetchedAt.Valid {
		if t, err := time.Parse(time.RFC3339Nano, row.CardFetchedAt.String); err == nil {
			u.CardFetchedAt = t
		}
	}
	return u
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: true}
}

// validateCard sanity-checks a fetched card.
func validateCard(c *a2a.AgentCard) error {
	if c == nil {
		return errors.New("card is nil")
	}
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("card.name is empty")
	}
	if strings.TrimSpace(c.URL) == "" {
		return errors.New("card.url is empty")
	}
	// Skills may be empty but must not be nil after unmarshal — go decodes
	// missing fields as zero-value ([]), which is fine.
	return nil
}

// HTTPFetcher is the default Fetcher: tries /.well-known/agent-card.json first,
// then /.well-known/agent.json for backward compat.
type HTTPFetcher struct {
	Client *http.Client
}

// Fetch implements Fetcher.
func (f *HTTPFetcher) Fetch(ctx context.Context, baseURL, scheme, token string) (*a2a.AgentCard, error) {
	if f.Client == nil {
		f.Client = &http.Client{Timeout: 5 * time.Second}
	}
	base := strings.TrimRight(baseURL, "/")
	paths := []string{"/.well-known/agent-card.json", "/.well-known/agent.json"}
	var lastErr error
	for _, p := range paths {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+p, nil)
		if err != nil {
			return nil, err
		}
		if scheme == "bearer" && token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := f.Client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("%s returned HTTP %d", p, resp.StatusCode)
			continue
		}
		var card a2a.AgentCard
		if err := json.Unmarshal(body, &card); err != nil {
			lastErr = fmt.Errorf("%s parse error: %w", p, err)
			continue
		}
		return &card, nil
	}
	if lastErr == nil {
		lastErr = errors.New("card not found at any well-known path")
	}
	return nil, lastErr
}
