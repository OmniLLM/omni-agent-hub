// Package transport hosts the HTTP surface of the omni-agent-hub hub.
//
// Handler-thinness invariant: every handler function does exactly three things:
//  1. parse the request into a strong type,
//  2. call ONE method on dispatch, registry, or card.Current(),
//  3. serialize and write the response.
//
// Anything more (routing, validation, business rules) belongs in the packages
// underneath.
package transport

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
	"github.com/OmniLLM/omni-agent-hub/internal/card"
	"github.com/OmniLLM/omni-agent-hub/internal/config"
	"github.com/OmniLLM/omni-agent-hub/internal/dispatch"
	"github.com/OmniLLM/omni-agent-hub/internal/registry"
	"github.com/OmniLLM/omni-agent-hub/internal/store"
)

// Deps bundles what a Server needs at construction time.
type Deps struct {
	Cfg     *config.Config
	Reg     registry.Registry
	Card    card.Builder
	Store   *store.Store
	Unary   dispatch.Unary
	Stream  dispatch.Stream
	Version string
}

// Server holds the mux and deps.
type Server struct {
	deps Deps
	mux  *http.ServeMux
}

// New builds a Server with routes wired.
func New(deps Deps) *Server {
	s := &Server{deps: deps, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler returns the top-level HTTP handler with CORS applied.
func (s *Server) Handler() http.Handler {
	return corsMiddleware(traceMiddleware(s.mux))
}

func (s *Server) routes() {
	// CORS preflight for anything.
	s.mux.HandleFunc("OPTIONS /", handleCORSPreflight)

	// Public discovery / health / metrics.
	s.mux.HandleFunc("GET /.well-known/agent-card.json", s.handleAgentCard)
	s.mux.HandleFunc("GET /.well-known/agent.json", s.handleAgentCard)
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)

	// Client A2A surface.
	s.mux.HandleFunc("POST /", s.clientAuth(s.handleJSONRPC))
	// Path-style compat shim (converts to message/send internally).
	s.mux.HandleFunc("POST /message:send", s.clientAuth(s.handleMessageSendCompat))
	// REST (A2A HTTP+JSON) binding: bare MessageSendParams in, raw Task / SSE out.
	s.mux.HandleFunc("POST /a2a/v1/message:send", s.clientAuth(s.handleRESTMessageSend))
	s.mux.HandleFunc("POST /a2a/v1/message:stream", s.clientAuth(s.handleRESTMessageStream))
	s.mux.HandleFunc("GET /a2a/v1/tasks/{id}", s.clientAuth(s.handleRESTGetTask))
	s.mux.HandleFunc("POST /a2a/v1/tasks/", s.clientAuth(s.handleRESTTaskAction))
	s.mux.HandleFunc("POST /message:stream", s.clientAuth(s.handleRESTMessageStream))

	// Admin surface.
	s.mux.HandleFunc("GET /admin/upstreams", s.adminAuth(s.handleAdminListUpstreams))
	s.mux.HandleFunc("POST /admin/upstreams", s.adminAuth(s.handleAdminAddUpstream))
	s.mux.HandleFunc("GET /admin/upstreams/{id}", s.adminAuth(s.handleAdminGetUpstream))
	s.mux.HandleFunc("DELETE /admin/upstreams/{id}", s.adminAuth(s.handleAdminRemoveUpstream))
	s.mux.HandleFunc("POST /admin/upstreams/{id}/refresh", s.adminAuth(s.handleAdminRefreshOne))
	s.mux.HandleFunc("POST /admin/upstreams/{id}/test", s.adminAuth(s.handleAdminTestUpstream))
	s.mux.HandleFunc("POST /admin/refresh", s.adminAuth(s.handleAdminRefreshAll))
	s.mux.HandleFunc("GET /admin/skills", s.adminAuth(s.handleAdminSkills))
	s.mux.HandleFunc("GET /admin/health", s.adminAuth(s.handleAdminHealth))
	s.mux.HandleFunc("GET /admin/tasks", s.adminAuth(s.handleAdminListTasks))
	s.mux.HandleFunc("GET /admin/tasks/{id}", s.adminAuth(s.handleAdminGetTask))
	s.mux.HandleFunc("POST /admin/tasks/{id}/cancel", s.adminAuth(s.handleAdminCancelTask))
	s.mux.HandleFunc("GET /admin/audit", s.adminAuth(s.handleAdminListAudit))
	s.mux.HandleFunc("POST /admin/messages", s.adminAuth(s.handleAdminSendMessage))
	s.mux.HandleFunc("GET /admin/version", s.adminAuth(s.handleAdminVersion))
}

// --- Middleware ------------------------------------------------------------

// contextKey is a small typed key for context values we stash per request.
type contextKey string

const traceIDKey contextKey = "trace_id"

func setCORSHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")
	h.Set("Access-Control-Max-Age", "86400")
}

func handleCORSPreflight(w http.ResponseWriter, _ *http.Request) {
	setCORSHeaders(w)
	w.WriteHeader(http.StatusNoContent)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setCORSHeaders(w)
		next.ServeHTTP(w, r)
	})
}

func traceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.NewString()
		ctx := context.WithValue(r.Context(), traceIDKey, id)
		w.Header().Set("X-Request-ID", id)
		slog.Debug("http request", "trace_id", id,
			"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func traceID(ctx context.Context) string {
	if v, ok := ctx.Value(traceIDKey).(string); ok {
		return v
	}
	return ""
}

func (s *Server) clientAuth(next http.HandlerFunc) http.HandlerFunc {
	return keyGuard(s.deps.Cfg.Server.APIKey, "client", next)
}

func (s *Server) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return keyGuard(s.deps.Cfg.Server.AdminKey, "admin", next)
}

// keyGuard requires that the request carry `expected` in either
// Authorization: Bearer or X-API-Key. Empty `expected` DENIES all requests
// (fail-safe: never accidentally open an unauthenticated endpoint).
func keyGuard(expected, kind string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if expected == "" {
			writeJSON(w, http.StatusUnauthorized,
				map[string]string{"error": kind + " key not configured; endpoint disabled"})
			return
		}
		got := extractBearer(r)
		if got == "" {
			got = r.Header.Get("X-API-Key")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
			writeJSON(w, http.StatusUnauthorized,
				map[string]string{"error": "unauthorized: invalid or missing " + kind + " key"})
			return
		}
		next(w, r)
	}
}

func extractBearer(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
		return auth[len(prefix):]
	}
	return ""
}

// --- Public handlers -------------------------------------------------------

func (s *Server) handleAgentCard(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.deps.Card.Current())
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	all := s.deps.Reg.List()
	healthy := 0
	for _, u := range all {
		if u.Status == store.StatusHealthy {
			healthy++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"upstreams": map[string]int{
			"total":   len(all),
			"healthy": healthy,
		},
	})
}

// --- Utility ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, http.StatusOK, a2a.JSONRPCResponse{
		JSONRPC: "2.0", ID: id, Result: result,
	})
}

func writeJSONRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string, data any) {
	writeJSON(w, http.StatusOK, a2a.JSONRPCResponse{
		JSONRPC: "2.0", ID: id,
		Error: &a2a.JSONRPCError{Code: code, Message: message, Data: data},
	})
}
