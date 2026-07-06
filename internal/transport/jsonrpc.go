package transport

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
	"github.com/OmniLLM/omni-agent-hub/internal/dispatch"
	"github.com/OmniLLM/omni-agent-hub/internal/registry"
	"github.com/OmniLLM/omni-agent-hub/internal/router"
)

// handleJSONRPC dispatches to the right method by name.
func (s *Server) handleJSONRPC(w http.ResponseWriter, r *http.Request) {
	// Cap request body to 10 MB to prevent unbounded memory allocation.
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONRPCError(w, nil, a2a.ErrParseError, "Parse error", err.Error())
		return
	}
	var req a2a.JSONRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONRPCError(w, nil, a2a.ErrParseError, "Parse error", err.Error())
		return
	}
	if req.JSONRPC != "2.0" {
		writeJSONRPCError(w, req.ID, a2a.ErrInvalidRequest, "Invalid Request", "jsonrpc must be '2.0'")
		return
	}
	switch req.Method {
	case "message/send":
		s.jsonrpcSendMessage(w, r, &req, false)
	case "message/sendSubscribe":
		s.jsonrpcSendMessage(w, r, &req, true)
	case "tasks/get":
		s.jsonrpcGetTask(w, r, &req)
	case "tasks/cancel":
		s.jsonrpcCancelTask(w, r, &req)
	case "agent/getAuthenticatedExtendedCard":
		writeJSONRPCResult(w, req.ID, s.deps.Card.Current())
	default:
		writeJSONRPCError(w, req.ID, a2a.ErrMethodNotFound, "Method not found", req.Method)
	}
}

// jsonrpcSendMessage handles both unary (message/send) and stream
// (message/sendSubscribe). When stream=true and dispatch returns a channel,
// this handler upgrades the response to SSE and relays events.
func (s *Server) jsonrpcSendMessage(w http.ResponseWriter, r *http.Request, req *a2a.JSONRPCRequest, stream bool) {
	var params a2a.SendMessageParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeJSONRPCError(w, req.ID, a2a.ErrInvalidParams, "Invalid params", err.Error())
		return
	}

	tid := traceID(r.Context())
	method := "message/send"
	if stream {
		method = "message/sendSubscribe"
	}

	slog.Debug("jsonrpc dispatch start",
		"trace_id", tid,
		"method", method,
		"skill_id", params.SkillID,
		"context_id", params.ContextID,
		"text_preview", truncate(params.Message.FirstText(), 80),
	)

	// Build a snapshot for the router. Sticky lookup goes through store.
	snap := s.snapshot(r, params.ContextID)
	res, ok := router.Resolve(router.Request{
		SkillID:   params.SkillID,
		Text:      params.Message.FirstText(),
		ContextID: params.ContextID,
	}, snap)
	if !ok {
		slog.Warn("jsonrpc no route",
			"trace_id", tid,
			"skill_id", params.SkillID,
			"text_preview", truncate(params.Message.FirstText(), 80),
		)
		writeJSONRPCError(w, req.ID, a2a.ErrNoRoute, "No route",
			"could not resolve request to any healthy upstream")
		return
	}

	// Resolve upstream name for logging.
	upstreamName := string(res.UpstreamID)
	if u, found := s.deps.Reg.Get(res.UpstreamID); found {
		upstreamName = u.Name
	}
	slog.Info("jsonrpc routed",
		"trace_id", tid,
		"method", method,
		"upstream", upstreamName,
		"upstream_id", res.UpstreamID,
		"upstream_skill", res.UpstreamSkillID,
		"route_reason", res.Reason,
		"skill_id", params.SkillID,
	)

	unaryReq := dispatch.UnaryRequest{
		Res:       res,
		Message:   params.Message,
		ContextID: params.ContextID,
		TraceID:   tid,
	}

	if stream {
		s.serveSSE(w, r, req.ID, unaryReq)
		return
	}
	resp, err := s.deps.Unary.SendMessage(r.Context(), unaryReq)
	if err != nil {
		slog.Warn("jsonrpc dispatch error",
			"trace_id", tid,
			"upstream", upstreamName,
			"err", err,
		)
		writeAnyJSONRPCError(w, req.ID, err)
		return
	}
	slog.Info("jsonrpc dispatch done",
		"trace_id", tid,
		"upstream", upstreamName,
		"task_id", resp.Task.TaskID,
		"state", resp.Task.Status.State,
	)
	writeJSONRPCResult(w, req.ID, resp.Task)
}

// serveSSE upgrades w to an event-stream response and pumps dispatch events.
func (s *Server) serveSSE(w http.ResponseWriter, r *http.Request, id json.RawMessage, req dispatch.UnaryRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONRPCError(w, id, a2a.ErrInternal, "Internal error", "response writer is not a flusher")
		return
	}
	ch, err := s.deps.Stream.SendMessageSubscribe(r.Context(), req)
	if err != nil {
		writeAnyJSONRPCError(w, id, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for evt := range ch {
		if _, err := w.Write([]byte("data: ")); err != nil {
			return
		}
		if _, err := w.Write(evt.Data); err != nil {
			return
		}
		if _, err := w.Write([]byte("\n\n")); err != nil {
			return
		}
		flusher.Flush()
	}
}

func (s *Server) jsonrpcGetTask(w http.ResponseWriter, r *http.Request, req *a2a.JSONRPCRequest) {
	var params a2a.GetTaskParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeJSONRPCError(w, req.ID, a2a.ErrInvalidParams, "Invalid params", err.Error())
		return
	}
	task, err := s.deps.Unary.GetTask(r.Context(), params.TaskID)
	if err != nil {
		writeAnyJSONRPCError(w, req.ID, err)
		return
	}
	writeJSONRPCResult(w, req.ID, task)
}

func (s *Server) jsonrpcCancelTask(w http.ResponseWriter, r *http.Request, req *a2a.JSONRPCRequest) {
	var params a2a.CancelTaskParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeJSONRPCError(w, req.ID, a2a.ErrInvalidParams, "Invalid params", err.Error())
		return
	}
	if err := s.deps.Unary.CancelTask(r.Context(), params.TaskID); err != nil {
		writeAnyJSONRPCError(w, req.ID, err)
		return
	}
	writeJSONRPCResult(w, req.ID, map[string]string{"status": "canceled"})
}

// handleMessageSendCompat implements the legacy path-style /message:send
// endpoint by translating into a message/send call. This keeps existing
// clients working during the migration to /.
func (s *Server) handleMessageSendCompat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []a2a.Message `json:"messages"`
		Tool     string        `json:"tool,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"code": a2a.ErrParseError, "message": "Parse error: " + err.Error(),
			},
		})
		return
	}
	if len(body.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"code": a2a.ErrInvalidParams, "message": "messages array is required",
			},
		})
		return
	}

	tid := traceID(r.Context())
	firstText := ""
	if len(body.Messages[0].Parts) > 0 {
		firstText = body.Messages[0].Parts[0].Text
	}
	slog.Debug("compat dispatch start",
		"trace_id", tid,
		"tool", body.Tool,
		"text_preview", truncate(firstText, 80),
	)

	snap := s.snapshot(r, "")
	res, ok := router.Resolve(router.Request{
		SkillID: body.Tool, Text: firstText,
	}, snap)
	if !ok {
		slog.Warn("compat no route",
			"trace_id", tid,
			"tool", body.Tool,
			"text_preview", truncate(firstText, 80),
		)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{
				"code": a2a.ErrNoRoute, "message": "No route",
			},
		})
		return
	}

	upstreamName := string(res.UpstreamID)
	if u, found := s.deps.Reg.Get(res.UpstreamID); found {
		upstreamName = u.Name
	}
	slog.Info("compat routed",
		"trace_id", tid,
		"upstream", upstreamName,
		"upstream_skill", res.UpstreamSkillID,
		"route_reason", res.Reason,
		"tool", body.Tool,
	)

	resp, err := s.deps.Unary.SendMessage(r.Context(), dispatch.UnaryRequest{
		Res: res, Message: body.Messages[0], TraceID: tid,
	})
	if err != nil {
		slog.Warn("compat dispatch error",
			"trace_id", tid,
			"upstream", upstreamName,
			"err", err,
		)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{
				"code": a2a.ErrUpstreamHTTP, "message": err.Error(),
			},
		})
		return
	}
	slog.Info("compat dispatch done",
		"trace_id", tid,
		"upstream", upstreamName,
		"task_id", resp.Task.TaskID,
		"state", resp.Task.Status.State,
	)
	writeJSON(w, http.StatusOK, resp.Task)
}

// snapshot assembles a router.Snapshot from the registry, adding sticky
// context lookup for the given contextId if non-empty.
func (s *Server) snapshot(r *http.Request, contextID string) router.Snapshot {
	snap := router.NewSnapshot(s.deps.Reg.List())
	if contextID == "" {
		return snap
	}
	if upID, ok := s.deps.Store.LookupContext(r.Context(), contextID); ok {
		snap.WithSticky(contextID, registry.UpstreamID(upID))
	}
	return snap
}

// writeAnyJSONRPCError writes an error response, unwrapping *a2a.JSONRPCError
// when possible so the wire code is preserved.
func writeAnyJSONRPCError(w http.ResponseWriter, id json.RawMessage, err error) {
	var rpcErr *a2a.JSONRPCError
	if errors.As(err, &rpcErr) {
		writeJSON(w, http.StatusOK, a2a.JSONRPCResponse{
			JSONRPC: "2.0", ID: id, Error: rpcErr,
		})
		return
	}
	writeJSONRPCError(w, id, a2a.ErrGeneric, "Execution error", err.Error())
}

// truncate returns the first n runes of s, appending "…" if truncated.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
