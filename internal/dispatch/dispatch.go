// Package dispatch forwards A2A requests to the correct upstream and hides
// task-ID translation, breaker checks, and SSE relay behind two small
// interfaces (Unary and Stream).
//
// This file implements the unary side: SendMessage, GetTask, CancelTask.
// The stream side lives in stream.go.
package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
	"github.com/OmniLLM/omni-agent-hub/internal/registry"
	"github.com/OmniLLM/omni-agent-hub/internal/router"
	"github.com/OmniLLM/omni-agent-hub/internal/store"
)

// UnaryRequest carries what the caller learned from the router plus the raw
// message pieces needed to build the upstream call.
type UnaryRequest struct {
	Res       router.Resolution
	Message   a2a.Message
	ContextID string
	TraceID   string
}

// UnaryResponse is the parsed upstream response with hub_task_id substituted
// for upstream_task_id.
type UnaryResponse struct {
	Task *a2a.Task
}

// Unary is the interface transport handlers call for non-streaming methods.
type Unary interface {
	SendMessage(ctx context.Context, req UnaryRequest) (UnaryResponse, error)
	GetTask(ctx context.Context, hubTaskID string) (*a2a.Task, error)
	CancelTask(ctx context.Context, hubTaskID string) error
}

// Stream is the interface for message/sendSubscribe (SSE relay).
type Stream interface {
	SendMessageSubscribe(ctx context.Context, req UnaryRequest) (<-chan StreamEvent, error)
}

// Dispatcher implements both Unary and Stream.
type Dispatcher struct {
	Reg             registry.Registry
	Store           *store.Store
	Client          *http.Client
	StreamTransport *http.Transport // shared transport for SSE streams (no timeout)
}

// New constructs a Dispatcher with sensible HTTP defaults.
func New(reg registry.Registry, s *store.Store) *Dispatcher {
	streamTransport := http.DefaultTransport.(*http.Transport).Clone()
	return &Dispatcher{
		Reg:   reg,
		Store: s,
		Client: &http.Client{
			// Per spec: 30s connect, 300s total response for unary.
			// net/http.Client uses a single Timeout (which is total, not
			// connect); the 300s value dominates.
			Timeout: 300 * time.Second,
		},
		StreamTransport: streamTransport,
	}
}

// SendMessage implements the unary send flow described in §8 of the spec.
func (d *Dispatcher) SendMessage(ctx context.Context, req UnaryRequest) (UnaryResponse, error) {
	prep, err := d.prepareSend(ctx, req, "message/send")
	if err != nil {
		return UnaryResponse{}, err
	}

	respBody, httpErr := d.postToUpstream(ctx, prep.Upstream, prep.Body, "application/json")
	if httpErr != nil {
		if !isClientError(httpErr) {
			d.Reg.RecordFailure(prep.Upstream.ID, httpErr)
		}
		_ = d.Store.UpdateTaskSnapshot(ctx, prep.HubTaskID, a2a.TaskStateFailed, nil)
		_ = d.Store.WriteAudit(ctx, store.AuditEntry{
			TraceID: req.TraceID, HubTaskID: prep.HubTaskID,
			UpstreamID: string(prep.Upstream.ID), Event: store.EventError,
			Detail: map[string]string{"err": httpErr.Error()},
		})
		return UnaryResponse{}, a2a.NewError(a2a.ErrUpstreamHTTP, "Upstream HTTP error", httpErr.Error())
	}

	var rpcResp a2a.JSONRPCResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		d.Reg.RecordFailure(prep.Upstream.ID, err)
		return UnaryResponse{}, a2a.NewError(a2a.ErrInvalidUpstream, "Invalid upstream response", err.Error())
	}
	if rpcResp.Error != nil {
		// Upstream returned a JSON-RPC error — pass through, don't count as
		// breaker failure (client-caused).
		_ = d.Store.UpdateTaskSnapshot(ctx, prep.HubTaskID, a2a.TaskStateFailed, nil)
		return UnaryResponse{}, rpcResp.Error
	}

	task, err := parseTaskResult(rpcResp.Result)
	if err != nil {
		d.Reg.RecordFailure(prep.Upstream.ID, err)
		return UnaryResponse{}, a2a.NewError(a2a.ErrInvalidUpstream, "Invalid upstream response", err.Error())
	}
	// Persist the upstream→hub task id mapping and rewrite before returning.
	upstreamTaskID := task.TaskID
	if err := d.Store.MapTaskID(ctx, prep.HubTaskID, string(prep.Upstream.ID), upstreamTaskID); err != nil {
		return UnaryResponse{}, fmt.Errorf("dispatch: map task id: %w", err)
	}
	task.TaskID = prep.HubTaskID
	_ = d.Store.UpdateTaskSnapshot(ctx, prep.HubTaskID, task.Status.State, task)
	_ = d.Store.WriteAudit(ctx, store.AuditEntry{
		TraceID: req.TraceID, HubTaskID: prep.HubTaskID,
		UpstreamID: string(prep.Upstream.ID), Event: store.EventResponse,
		Detail: map[string]string{"state": string(task.Status.State)},
	})
	d.Reg.RecordSuccess(prep.Upstream.ID)
	return UnaryResponse{Task: task}, nil
}

// GetTask implements the unary get flow.
func (d *Dispatcher) GetTask(ctx context.Context, hubTaskID string) (*a2a.Task, error) {
	row, err := d.Store.GetTask(ctx, hubTaskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, a2a.NewError(a2a.ErrTaskNotFound, "Task not found", hubTaskID)
	}
	if err != nil {
		return nil, err
	}
	// If terminal, return cached snapshot.
	if row.State.IsTerminal() {
		if t := row.Task(); t != nil {
			return t, nil
		}
	}
	upstreamTaskID, err := d.Store.LookupUpstreamTaskID(ctx, hubTaskID)
	if errors.Is(err, store.ErrNotFound) {
		// Task was created but never mapped (e.g. upstream never returned) —
		// fall back to whatever snapshot we have, or empty.
		if t := row.Task(); t != nil {
			return t, nil
		}
		return nil, a2a.NewError(a2a.ErrTaskNotFound, "Task not found", hubTaskID)
	}
	if err != nil {
		return nil, err
	}
	up, err := d.upstreamFor(registry.UpstreamID(row.UpstreamID))
	if err != nil {
		return nil, err
	}
	// Forward tasks/get with the upstream ID.
	task, err := d.forwardTaskCall(ctx, up, "tasks/get", upstreamTaskID)
	if err != nil {
		return nil, err
	}
	task.TaskID = hubTaskID
	_ = d.Store.UpdateTaskSnapshot(ctx, hubTaskID, task.Status.State, task)
	return task, nil
}

// CancelTask forwards a cancel to the owning upstream, or reports task not
// found if the row doesn't exist.
func (d *Dispatcher) CancelTask(ctx context.Context, hubTaskID string) error {
	row, err := d.Store.GetTask(ctx, hubTaskID)
	if errors.Is(err, store.ErrNotFound) {
		return a2a.NewError(a2a.ErrTaskNotFound, "Task not found", hubTaskID)
	}
	if err != nil {
		return err
	}
	upstreamTaskID, err := d.Store.LookupUpstreamTaskID(ctx, hubTaskID)
	if errors.Is(err, store.ErrNotFound) {
		return a2a.NewError(a2a.ErrTaskNotFound, "Task not found", hubTaskID)
	}
	if err != nil {
		return err
	}
	up, err := d.upstreamFor(registry.UpstreamID(row.UpstreamID))
	if err != nil {
		return err
	}
	task, err := d.forwardTaskCall(ctx, up, "tasks/cancel", upstreamTaskID)
	if err != nil {
		// If the upstream no longer knows this task (e.g. it restarted and
		// lost ephemeral state), treat it as a successful cancel — the task
		// is gone upstream, so we mark it canceled locally.
		var rpcErr *a2a.JSONRPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == a2a.ErrTaskNotFound {
			_ = d.Store.UpdateTaskSnapshot(ctx, hubTaskID, a2a.TaskStateCanceled, nil)
			_ = d.Store.WriteAudit(ctx, store.AuditEntry{
				HubTaskID: hubTaskID, UpstreamID: row.UpstreamID, Event: store.EventCancel,
				Detail: map[string]string{"note": "upstream task not found, marked canceled locally"},
			})
			return nil
		}
		return err
	}
	task.TaskID = hubTaskID
	_ = d.Store.UpdateTaskSnapshot(ctx, hubTaskID, task.Status.State, task)
	_ = d.Store.WriteAudit(ctx, store.AuditEntry{
		HubTaskID: hubTaskID, UpstreamID: row.UpstreamID, Event: store.EventCancel,
	})
	return nil
}

// --- helpers ---------------------------------------------------------------

// upstreamFor returns the registry snapshot for id, or a hub error if absent.
func (d *Dispatcher) upstreamFor(id registry.UpstreamID) (registry.Upstream, error) {
	u, ok := d.Reg.Get(id)
	if !ok {
		return registry.Upstream{}, a2a.NewError(a2a.ErrNoRoute, "No route", fmt.Sprintf("upstream %q not registered", id))
	}
	return u, nil
}

// httpClientError marks errors caused by client-side issues (4xx) that should
// NOT count against the upstream's circuit breaker.
type httpClientError struct {
	err error
}

func (e *httpClientError) Error() string { return e.err.Error() }
func (e *httpClientError) Unwrap() error { return e.err }

// isClientError reports whether err is a client-caused HTTP error (4xx) that
// should not penalize the upstream's circuit breaker.
func isClientError(err error) bool {
	var ce *httpClientError
	return errors.As(err, &ce)
}

// postToUpstream sends body to the upstream and returns the response body on
// HTTP 200. Any non-2xx / network error is returned as an error suitable for
// wrapping into a JSON-RPC error and for RecordFailure.
// 4xx errors are wrapped in httpClientError so callers can distinguish them
// from infrastructure failures and avoid penalizing the circuit breaker.
func (d *Dispatcher) postToUpstream(ctx context.Context, u registry.Upstream, body []byte, accept string) ([]byte, error) {
	target := strings.TrimRight(u.BaseURL, "/") + "/"
	slog.Debug("upstream POST",
		"upstream", u.Name,
		"target", target,
		"body_len", len(body),
	)
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if u.Auth.Scheme == "bearer" && u.Auth.Token != "" {
		req.Header.Set("Authorization", "Bearer "+u.Auth.Token)
	}
	resp, err := d.Client.Do(req)
	if err != nil {
		slog.Warn("upstream POST failed",
			"upstream", u.Name,
			"target", target,
			"elapsed_ms", time.Since(start).Milliseconds(),
			"err", err,
		)
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	slog.Debug("upstream POST response",
		"upstream", u.Name,
		"status", resp.StatusCode,
		"elapsed_ms", time.Since(start).Milliseconds(),
		"body_len", len(respBody),
	)
	if resp.StatusCode/100 == 5 || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504 {
		return nil, fmt.Errorf("upstream returned HTTP %d: %s", resp.StatusCode, summarizeBody(respBody))
	}
	if resp.StatusCode/100 != 2 {
		// 4xx is generally client-caused; wrap so callers can skip breaker penalty.
		return nil, &httpClientError{err: fmt.Errorf("upstream returned HTTP %d: %s", resp.StatusCode, summarizeBody(respBody))}
	}
	return respBody, nil
}

func summarizeBody(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "empty response body"
	}
	trimmed = strings.ReplaceAll(trimmed, "\n", "\\n")
	trimmed = strings.ReplaceAll(trimmed, "\r", "")
	const max = 160
	if len(trimmed) > max {
		return trimmed[:max] + "..."
	}
	return trimmed
}

// forwardTaskCall makes a tasks/get or tasks/cancel call to the upstream and
// parses the Task result.
func (d *Dispatcher) forwardTaskCall(ctx context.Context, u registry.Upstream, method, upstreamTaskID string) (*a2a.Task, error) {
	params, _ := json.Marshal(a2a.GetTaskParams{TaskID: upstreamTaskID})
	rpcReq := a2a.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(fmt.Sprintf("%q", uuid.NewString())),
		Method:  method,
		Params:  params,
	}
	body, err := json.Marshal(rpcReq)
	if err != nil {
		return nil, err
	}
	respBody, err := d.postToUpstream(ctx, u, body, "")
	if err != nil {
		d.Reg.RecordFailure(u.ID, err)
		return nil, a2a.NewError(a2a.ErrUpstreamHTTP, "Upstream HTTP error", err.Error())
	}
	var rpcResp a2a.JSONRPCResponse
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		d.Reg.RecordFailure(u.ID, err)
		return nil, a2a.NewError(a2a.ErrInvalidUpstream, "Invalid upstream response", err.Error())
	}
	if rpcResp.Error != nil {
		return nil, rpcResp.Error
	}
	task, err := parseTaskResult(rpcResp.Result)
	if err != nil {
		d.Reg.RecordFailure(u.ID, err)
		return nil, a2a.NewError(a2a.ErrInvalidUpstream, "Invalid upstream response", err.Error())
	}
	d.Reg.RecordSuccess(u.ID)
	return task, nil
}

// parseTaskResult converts the "any" Result of a JSON-RPC response into an
// *a2a.Task by round-tripping through JSON.
func parseTaskResult(result any) (*a2a.Task, error) {
	if result == nil {
		return nil, errors.New("result is empty")
	}
	b, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	var t a2a.Task
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	if t.TaskID == "" {
		return nil, errors.New("result missing task id")
	}
	return &t, nil
}
