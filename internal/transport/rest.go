package transport

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
	"github.com/OmniLLM/omni-agent-hub/internal/dispatch"
)

// REST (A2A HTTP+JSON) binding.
//
// These handlers implement the path-style A2A transport for clients that POST a
// bare MessageSendParams body (no JSON-RPC envelope) and expect a raw Task
// object (message:send) or a raw A2A event SSE stream (message:stream):
//
//	POST /a2a/v1/message:send
//	POST /a2a/v1/message:stream
//	POST /message:stream           (root alias; pairs with the legacy /message:send shim)
//
// They share the router + dispatch core with the JSON-RPC surface (see
// jsonrpc.go). Only request parsing and error framing differ: a bare params
// body in, plain-HTTP JSON errors out instead of JSON-RPC envelopes.

// handleRESTMessageSend implements POST /a2a/v1/message:send. Response is the
// raw Task JSON (HTTP 200), matching the A2A HTTP+JSON binding.
func (s *Server) handleRESTMessageSend(w http.ResponseWriter, r *http.Request) {
	params, ok := decodeRESTParams(w, r)
	if !ok {
		return
	}
	tid := traceID(r.Context())
	res, upstreamName, ok := s.route(r, params)
	if !ok {
		slog.Warn("rest no route", "trace_id", tid, "skill_id", params.SkillID,
			"text_preview", truncate(params.Message.FirstText(), 80))
		restError(w, http.StatusBadGateway, a2a.ErrNoRoute,
			"could not resolve request to any healthy upstream")
		return
	}
	slog.Info("rest routed", "trace_id", tid, "method", "message/send",
		"upstream", upstreamName, "upstream_skill", res.UpstreamSkillID,
		"route_reason", res.Reason)

	resp, err := s.deps.Unary.SendMessage(r.Context(), dispatch.UnaryRequest{
		Res: res, Message: params.Message, ContextID: params.ContextID, TraceID: tid,
	})
	if err != nil {
		slog.Warn("rest dispatch error", "trace_id", tid, "upstream", upstreamName, "err", err)
		restDispatchError(w, err)
		return
	}
	slog.Info("rest dispatch done", "trace_id", tid, "upstream", upstreamName,
		"task_id", resp.Task.TaskID, "state", resp.Task.Status.State)
	writeJSON(w, http.StatusOK, resp.Task)
}

// handleRESTMessageStream implements POST /a2a/v1/message:stream and the root
// /message:stream alias. Response is an SSE stream of raw A2A events.
func (s *Server) handleRESTMessageStream(w http.ResponseWriter, r *http.Request) {
	params, ok := decodeRESTParams(w, r)
	if !ok {
		return
	}
	tid := traceID(r.Context())
	res, upstreamName, ok := s.route(r, params)
	if !ok {
		slog.Warn("rest no route", "trace_id", tid, "skill_id", params.SkillID,
			"text_preview", truncate(params.Message.FirstText(), 80))
		restError(w, http.StatusBadGateway, a2a.ErrNoRoute,
			"could not resolve request to any healthy upstream")
		return
	}
	slog.Info("rest routed", "trace_id", tid, "method", "message/sendSubscribe",
		"upstream", upstreamName, "upstream_skill", res.UpstreamSkillID,
		"route_reason", res.Reason)

	flusher, ok := w.(http.Flusher)
	if !ok {
		restError(w, http.StatusInternalServerError, a2a.ErrInternal,
			"response writer is not a flusher")
		return
	}
	ch, err := s.deps.Stream.SendMessageSubscribe(r.Context(), dispatch.UnaryRequest{
		Res: res, Message: params.Message, ContextID: params.ContextID, TraceID: tid,
	})
	if err != nil {
		restDispatchError(w, err)
		return
	}
	streamSSE(w, flusher, ch)
}

// handleRESTGetTask implements GET /a2a/v1/tasks/{id}. Response is the raw Task
// JSON, using the same hub-task-id semantics as JSON-RPC tasks/get.
func (s *Server) handleRESTGetTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	if taskID == "" {
		restError(w, http.StatusBadRequest, a2a.ErrInvalidParams, "task id is required")
		return
	}

	task, err := s.deps.Unary.GetTask(r.Context(), taskID)
	if err != nil {
		restDispatchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// handleRESTTaskAction implements POST /a2a/v1/tasks/{id}:cancel. The standard
// ServeMux does not support a wildcard followed by a literal suffix, so the
// subtree route is parsed here and unknown task actions return 404.
func (s *Server) handleRESTTaskAction(w http.ResponseWriter, r *http.Request) {
	const prefix = "/a2a/v1/tasks/"
	const cancelSuffix = ":cancel"

	path := strings.TrimPrefix(r.URL.Path, prefix)
	if !strings.HasSuffix(path, cancelSuffix) {
		restError(w, http.StatusNotFound, a2a.ErrMethodNotFound, "task action not found")
		return
	}

	taskID := strings.TrimSuffix(path, cancelSuffix)
	if taskID == "" || strings.Contains(taskID, "/") {
		restError(w, http.StatusBadRequest, a2a.ErrInvalidParams, "task id is required")
		return
	}

	if err := s.deps.Unary.CancelTask(r.Context(), taskID); err != nil {
		restDispatchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "canceled"})
}

// decodeRESTParams reads a bare A2A MessageSendParams body ({"message":{...}}).
// On failure it writes a plain-HTTP error and returns ok=false.
func decodeRESTParams(w http.ResponseWriter, r *http.Request) (a2a.SendMessageParams, bool) {
	// Cap request body to 10 MB to prevent unbounded memory allocation.
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	var params a2a.SendMessageParams
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		restError(w, http.StatusBadRequest, a2a.ErrParseError, "Parse error: "+err.Error())
		return a2a.SendMessageParams{}, false
	}
	if len(params.Message.Parts) == 0 {
		restError(w, http.StatusBadRequest, a2a.ErrInvalidParams, "message.parts is required")
		return a2a.SendMessageParams{}, false
	}
	return params, true
}

// restError writes a plain-HTTP JSON error (no JSON-RPC envelope). code is the
// A2A wire code carried in the body for clients that inspect it.
func restError(w http.ResponseWriter, status, code int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// restDispatchError maps a dispatch error onto an HTTP status + JSON error body,
// preserving the A2A wire code when the error is a *a2a.JSONRPCError.
func restDispatchError(w http.ResponseWriter, err error) {
	var rpcErr *a2a.JSONRPCError
	if errors.As(err, &rpcErr) {
		restError(w, restStatusForCode(rpcErr.Code), rpcErr.Code, rpcErr.Message)
		return
	}
	restError(w, http.StatusInternalServerError, a2a.ErrGeneric, err.Error())
}

func restStatusForCode(code int) int {
	switch code {
	case a2a.ErrParseError, a2a.ErrInvalidRequest, a2a.ErrInvalidParams:
		return http.StatusBadRequest
	case a2a.ErrMethodNotFound, a2a.ErrTaskNotFound:
		return http.StatusNotFound
	case a2a.ErrUnavailable:
		return http.StatusServiceUnavailable
	case a2a.ErrUpstreamHTTP, a2a.ErrInvalidUpstream, a2a.ErrNoRoute:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}
