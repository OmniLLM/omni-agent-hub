package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
	"github.com/OmniLLM/omni-agent-hub/internal/config"
	"github.com/OmniLLM/omni-agent-hub/internal/dispatch"
	"github.com/OmniLLM/omni-agent-hub/internal/registry"
	"github.com/OmniLLM/omni-agent-hub/internal/router"
	"github.com/OmniLLM/omni-agent-hub/internal/store"
)

// upstreamInfoJSON is the shape returned by /admin/upstreams (list + create).
type upstreamInfoJSON struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	BaseURL  string      `json:"base_url"`
	Prefix   string      `json:"prefix,omitempty"`
	Enabled  bool        `json:"enabled"`
	Source   string      `json:"source"`
	Status   string      `json:"status"`
	HasCard  bool        `json:"has_card"`
	Skills   int         `json:"skills"`
}

func upstreamToInfo(u registry.Upstream) upstreamInfoJSON {
	info := upstreamInfoJSON{
		ID:      string(u.ID),
		Name:    u.Name,
		BaseURL: u.BaseURL,
		Prefix:  u.Prefix,
		Enabled: u.Enabled,
		Source:  string(u.Source),
		Status:  string(u.Status),
	}
	if u.Card != nil {
		info.HasCard = true
		info.Skills = len(u.Card.Skills)
	}
	return info
}

func (s *Server) handleAdminListUpstreams(w http.ResponseWriter, _ *http.Request) {
	list := s.deps.Reg.List()
	out := make([]upstreamInfoJSON, 0, len(list))
	for _, u := range list {
		out = append(out, upstreamToInfo(u))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAdminAddUpstream(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string             `json:"name"`
		BaseURL string             `json:"base_url"`
		Prefix  string             `json:"prefix,omitempty"`
		Auth    config.AuthConfig  `json:"auth"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if body.Auth.Scheme == "" {
		body.Auth.Scheme = "none"
	}
	up, err := s.deps.Reg.Add(r.Context(), registry.AddInput{
		Name: body.Name, BaseURL: body.BaseURL, Prefix: body.Prefix, Auth: body.Auth,
	})
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, registry.ErrDuplicateName) {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, upstreamToInfo(up))
}

func (s *Server) handleAdminUpsertUpstream(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string            `json:"name"`
		BaseURL string            `json:"base_url"`
		Prefix  string            `json:"prefix,omitempty"`
		Auth    config.AuthConfig `json:"auth"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if body.Auth.Scheme == "" {
		body.Auth.Scheme = "none"
	}

	status := http.StatusCreated
	if existing, ok := s.deps.Reg.GetByName(body.Name); ok {
		if err := s.deps.Reg.Remove(r.Context(), existing.ID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		status = http.StatusOK
	}

	up, err := s.deps.Reg.Add(r.Context(), registry.AddInput{
		Name: body.Name, BaseURL: body.BaseURL, Prefix: body.Prefix, Auth: body.Auth,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, status, upstreamToInfo(up))
}

func (s *Server) handleAdminRemoveUpstream(w http.ResponseWriter, r *http.Request) {
	id := registry.UpstreamID(r.PathValue("id"))
	if err := s.deps.Reg.Remove(r.Context(), id); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, registry.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdminRefreshOne(w http.ResponseWriter, r *http.Request) {
	id := registry.UpstreamID(r.PathValue("id"))
	// Use a detached context so the refresh (and its DB writes) complete
	// even if the HTTP client disconnects or times out.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	if err := s.deps.Reg.RefreshCard(ctx, id); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	u, _ := s.deps.Reg.Get(id)
	if u.Card == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "refreshed"})
		return
	}
	writeJSON(w, http.StatusOK, u.Card)
}

func (s *Server) handleAdminRefreshAll(w http.ResponseWriter, r *http.Request) {
	// Use a detached context so the refresh (and its DB writes) complete
	// even if the HTTP client disconnects or times out.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 60*time.Second)
	defer cancel()
	_ = s.deps.Reg.RefreshAll(ctx)
	writeJSON(w, http.StatusOK, map[string]string{"status": "refreshed"})
}

func (s *Server) handleAdminSkills(w http.ResponseWriter, _ *http.Request) {
	type skillRow struct {
		SkillID      string `json:"skill_id"`
		LocalSkillID string `json:"local_skill_id"`
		Name         string `json:"name"`
		Description  string `json:"description"`
		Upstream     string `json:"upstream"`
		UpstreamID   string `json:"upstream_id"`
		Status       string `json:"status"`
	}
	var out []skillRow
	for _, u := range s.deps.Reg.List() {
		if u.Card == nil {
			continue
		}
		for _, sk := range u.Card.Skills {
			out = append(out, skillRow{
				SkillID:      u.Name + "." + sk.ID,
				LocalSkillID: sk.ID,
				Name:         sk.Name,
				Description:  sk.Description,
				Upstream:     u.Name,
				UpstreamID:   string(u.ID),
				Status:       string(u.Status),
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// --- Health dashboard -------------------------------------------------------

func (s *Server) handleAdminHealth(w http.ResponseWriter, _ *http.Request) {
	all := s.deps.Reg.List()

	type upstreamHealth struct {
		ID                  string `json:"id"`
		Name                string `json:"name"`
		BaseURL             string `json:"base_url"`
		Prefix              string `json:"prefix,omitempty"`
		Enabled             bool   `json:"enabled"`
		Source              string `json:"source"`
		Status              string `json:"status"`
		ConsecutiveFailures int    `json:"consecutive_failures"`
		LastSuccessAt       string `json:"last_success_at,omitempty"`
		LastFailureAt       string `json:"last_failure_at,omitempty"`
		CardFetchedAt       string `json:"card_fetched_at,omitempty"`
		SkillCount          int    `json:"skill_count"`
	}
	type summary struct {
		Total     int `json:"total"`
		Healthy   int `json:"healthy"`
		Unhealthy int `json:"unhealthy"`
		Unknown   int `json:"unknown"`
		Enabled   int `json:"enabled"`
	}

	ups := make([]upstreamHealth, 0, len(all))
	var sum summary
	for _, u := range all {
		skills := 0
		if u.Card != nil {
			skills = len(u.Card.Skills)
		}
		uh := upstreamHealth{
			ID:                  string(u.ID),
			Name:                u.Name,
			BaseURL:             u.BaseURL,
			Prefix:              u.Prefix,
			Enabled:             u.Enabled,
			Source:              string(u.Source),
			Status:              string(u.Status),
			ConsecutiveFailures: u.ConsecutiveFailures,
			SkillCount:          skills,
		}
		if !u.LastSuccessAt.IsZero() {
			uh.LastSuccessAt = u.LastSuccessAt.Format(time.RFC3339)
		}
		if !u.LastFailureAt.IsZero() {
			uh.LastFailureAt = u.LastFailureAt.Format(time.RFC3339)
		}
		if !u.CardFetchedAt.IsZero() {
			uh.CardFetchedAt = u.CardFetchedAt.Format(time.RFC3339)
		}
		ups = append(ups, uh)

		sum.Total++
		if u.Enabled {
			sum.Enabled++
		}
		switch u.Status {
		case store.StatusHealthy:
			sum.Healthy++
		case store.StatusUnhealthy:
			sum.Unhealthy++
		default:
			sum.Unknown++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"upstreams": ups,
		"summary":   sum,
	})
}

// --- Upstream detail & test -------------------------------------------------

func (s *Server) handleAdminGetUpstream(w http.ResponseWriter, r *http.Request) {
	id := registry.UpstreamID(r.PathValue("id"))
	u, ok := s.deps.Reg.Get(id)
	if !ok {
		// Try by name.
		u, ok = s.deps.Reg.GetByName(string(id))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "upstream not found"})
			return
		}
	}

	type authInfo struct {
		Scheme    string `json:"scheme"`
		TokenHint string `json:"token_hint,omitempty"`
	}
	auth := authInfo{Scheme: u.Auth.Scheme}
	if u.Auth.Token != "" {
		if len(u.Auth.Token) > 8 {
			auth.TokenHint = u.Auth.Token[:8] + "…"
		} else {
			auth.TokenHint = u.Auth.Token
		}
	}

	out := map[string]any{
		"id":                    string(u.ID),
		"name":                  u.Name,
		"base_url":              u.BaseURL,
		"prefix":                u.Prefix,
		"enabled":               u.Enabled,
		"source":                string(u.Source),
		"status":                string(u.Status),
		"consecutive_failures":  u.ConsecutiveFailures,
		"auth":                  auth,
	}
	if !u.LastSuccessAt.IsZero() {
		out["last_success_at"] = u.LastSuccessAt.Format(time.RFC3339)
	}
	if !u.LastFailureAt.IsZero() {
		out["last_failure_at"] = u.LastFailureAt.Format(time.RFC3339)
	}
	if !u.CardFetchedAt.IsZero() {
		out["card_fetched_at"] = u.CardFetchedAt.Format(time.RFC3339)
	}
	if u.Card != nil {
		out["card"] = u.Card
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAdminTestUpstream(w http.ResponseWriter, r *http.Request) {
	id := registry.UpstreamID(r.PathValue("id"))
	u, ok := s.deps.Reg.Get(id)
	if !ok {
		u, ok = s.deps.Reg.GetByName(string(id))
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "upstream not found"})
			return
		}
	}

	base := strings.TrimRight(u.BaseURL, "/")
	cardURL := base + "/.well-known/agent-card.json"

	type testResult struct {
		OK         bool   `json:"ok"`
		UpstreamID string `json:"upstream_id"`
		BaseURL    string `json:"base_url"`
		CardURL    string `json:"card_url"`
		StatusCode int    `json:"status_code"`
		LatencyMS  int64  `json:"latency_ms"`
		HasCard    bool   `json:"has_card"`
		SkillCount int    `json:"skill_count"`
		Error      string `json:"error,omitempty"`
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cardURL, nil)
	if err != nil {
		writeJSON(w, http.StatusOK, testResult{
			UpstreamID: string(u.ID), BaseURL: u.BaseURL, CardURL: cardURL,
			Error: err.Error(),
		})
		return
	}
	if u.Auth.Scheme == "bearer" && u.Auth.Token != "" {
		req.Header.Set("Authorization", "Bearer "+u.Auth.Token)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		writeJSON(w, http.StatusOK, testResult{
			UpstreamID: string(u.ID), BaseURL: u.BaseURL, CardURL: cardURL,
			LatencyMS: latency, Error: err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	result := testResult{
		UpstreamID: string(u.ID),
		BaseURL:    u.BaseURL,
		CardURL:    cardURL,
		StatusCode: resp.StatusCode,
		LatencyMS:  latency,
		OK:         resp.StatusCode == http.StatusOK,
	}

	if resp.StatusCode == http.StatusOK {
		var card a2a.AgentCard
		if err := json.NewDecoder(resp.Body).Decode(&card); err == nil {
			result.HasCard = true
			result.SkillCount = len(card.Skills)
		}
	} else {
		result.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	writeJSON(w, http.StatusOK, result)
}

// --- Tasks API ---------------------------------------------------------------

func (s *Server) handleAdminListTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.TaskListFilter{
		UpstreamID: q.Get("upstream_id"),
		ContextID:  q.Get("context_id"),
		Recent:     q.Get("recent") == "true",
		Limit:      queryInt(q, "limit", 50),
		Offset:     queryInt(q, "offset", 0),
	}
	if states := q.Get("state"); states != "" {
		for _, s := range strings.Split(states, ",") {
			f.States = append(f.States, a2a.TaskState(strings.TrimSpace(s)))
		}
	}

	items, err := s.deps.Store.ListTasks(r.Context(), f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	total, _ := s.deps.Store.CountTasks(r.Context(), f)
	counts, _ := s.deps.Store.CountTasksByState(r.Context())

	if items == nil {
		items = []store.TaskListRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":  items,
		"total":  total,
		"limit":  f.Limit,
		"offset": f.Offset,
		"counts": counts,
	})
}

func (s *Server) handleAdminGetTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	detail, err := s.deps.Store.GetTaskDetail(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	out := map[string]any{
		"hub_task_id":      detail.Task.HubTaskID,
		"context_id":       detail.Task.ContextID,
		"upstream_id":      detail.Task.UpstreamID,
		"upstream_task_id": detail.UpstreamTaskID,
		"state":            detail.Task.State,
		"created_at":       detail.Task.CreatedAt,
		"updated_at":       detail.Task.UpdatedAt,
	}
	if t := detail.Task.Task(); t != nil {
		out["task"] = t
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAdminCancelTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.deps.Unary.CancelTask(r.Context(), id); err != nil {
		var rpcErr *a2a.JSONRPCError
		if errors.As(err, &rpcErr) {
			status := http.StatusBadGateway
			if rpcErr.Code == a2a.ErrTaskNotFound {
				status = http.StatusNotFound
			}
			writeJSON(w, status, map[string]string{"error": rpcErr.Message})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":      "cancel-requested",
		"hub_task_id": id,
	})
}

// --- Audit API ---------------------------------------------------------------

func (s *Server) handleAdminListAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.AuditListFilter{
		UpstreamID: q.Get("upstream_id"),
		HubTaskID:  q.Get("hub_task_id"),
		TraceID:    q.Get("trace_id"),
		Limit:      queryInt(q, "limit", 50),
		Offset:     queryInt(q, "offset", 0),
	}
	if ev := q.Get("event"); ev != "" {
		f.Event = store.AuditEvent(ev)
	}

	items, err := s.deps.Store.ListAudit(r.Context(), f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	total, _ := s.deps.Store.CountAudit(r.Context(), f)

	if items == nil {
		items = []store.AuditListRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":  items,
		"total":  total,
		"limit":  f.Limit,
		"offset": f.Offset,
	})
}

// --- Message send (admin manual dispatch) ------------------------------------

func (s *Server) handleAdminSendMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UpstreamID string `json:"upstream_id"`
		Message    string `json:"message"`
		ContextID  string `json:"context_id"`
		SkillID    string `json:"skill_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if body.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}

	// Resolve upstream: if UpstreamID given, route directly; otherwise use router.
	var res router.Resolution
	if body.UpstreamID != "" {
		upID := registry.UpstreamID(body.UpstreamID)
		// Also try by name.
		if _, ok := s.deps.Reg.Get(upID); !ok {
			if u, ok := s.deps.Reg.GetByName(body.UpstreamID); ok {
				upID = u.ID
			}
		}
		res = router.Resolution{
			UpstreamID:      upID,
			UpstreamSkillID: body.SkillID,
			Reason:          "admin",
		}
	} else {
		snap := router.NewSnapshot(s.deps.Reg.List())
		resolved, ok := router.Resolve(router.Request{
			SkillID: body.SkillID,
			Text:    body.Message,
		}, snap)
		if !ok {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "no route for message"})
			return
		}
		res = resolved
	}

	msg := a2a.Message{
		Role:  a2a.RoleUser,
		Parts: []a2a.Part{{Type: "text", Text: body.Message}},
	}
	tid := traceID(r.Context())
	resp, err := s.deps.Unary.SendMessage(r.Context(), dispatch.UnaryRequest{
		Res: res, Message: msg, ContextID: body.ContextID, TraceID: tid,
	})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"upstream_id": string(res.UpstreamID),
		"hub_task_id": resp.Task.TaskID,
		"context_id":  resp.Task.ContextID,
		"result":      resp.Task,
	})
}

// --- Version -----------------------------------------------------------------

func (s *Server) handleAdminVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"version": s.deps.Version,
	})
}

// --- Query param helpers -----------------------------------------------------

func queryInt(q interface{ Get(string) string }, key string, def int) int {
	v := q.Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}
