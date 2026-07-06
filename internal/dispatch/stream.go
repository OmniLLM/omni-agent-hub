package dispatch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/OmniLLM/omni-agent-hub/internal/a2a"
	"github.com/OmniLLM/omni-agent-hub/internal/store"
)

// StreamEvent is a single SSE event bound for the client.
type StreamEvent struct {
	// Data is the raw JSON payload to write after `data: `.
	Data []byte
	// Err is set if the upstream disconnected mid-stream. When Err != nil,
	// the transport should synthesize a `TaskStatusUpdateEvent{state:"failed"}`
	// and close the stream. (The dispatcher does this synthesis before closing
	// the channel so consumers don't need to.)
	Err error
	// Final indicates the last event in the stream; consumers should close
	// after writing it.
	Final bool
}

// SendMessageSubscribe forwards a message/sendSubscribe to the upstream and
// streams events back on the returned channel. The channel is closed when:
//   - the upstream sends a terminal event (state: completed/failed/canceled),
//   - the upstream disconnects mid-stream (a synthetic 'failed' event is emitted
//     first),
//   - the caller's ctx is cancelled.
func (d *Dispatcher) SendMessageSubscribe(ctx context.Context, req UnaryRequest) (<-chan StreamEvent, error) {
	prep, err := d.prepareSend(ctx, req, "message/sendSubscribe")
	if err != nil {
		return nil, err
	}
	// Streams get their own audit event on top of the shared "send" audit.
	_ = d.Store.WriteAudit(ctx, store.AuditEntry{
		TraceID: req.TraceID, HubTaskID: prep.HubTaskID,
		UpstreamID: string(prep.Upstream.ID), Event: store.EventStreamStart,
	})

	target := strings.TrimRight(prep.Upstream.BaseURL, "/") + "/"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(prep.Body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("A2A-Version", "1.0")
	if prep.Upstream.Auth.Scheme == "bearer" && prep.Upstream.Auth.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+prep.Upstream.Auth.Token)
	}
	// Streams use the shared transport but without the unary timeout cap.
	streamClient := &http.Client{Transport: d.StreamTransport, Timeout: 0}
	resp, err := streamClient.Do(httpReq)
	if err != nil {
		d.Reg.RecordFailure(prep.Upstream.ID, err)
		return nil, a2a.NewError(a2a.ErrUpstreamHTTP, "Upstream HTTP error", err.Error())
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		d.Reg.RecordFailure(prep.Upstream.ID, errors.New(resp.Status))
		return nil, a2a.NewError(a2a.ErrUpstreamHTTP, "Upstream HTTP error", resp.Status)
	}
	d.Reg.RecordSuccess(prep.Upstream.ID)
	slog.Info("stream connected",
		"trace_id", req.TraceID,
		"upstream", prep.Upstream.Name,
		"hub_task_id", prep.HubTaskID,
		"upstream_skill", req.Res.UpstreamSkillID,
	)

	// Unbuffered channel so the goroutine backpressures on the transport.
	ch := make(chan StreamEvent)
	go d.pipeSSE(ctx, resp.Body, ch, prep.HubTaskID, string(prep.Upstream.ID))
	return ch, nil
}

// pipeSSE reads SSE events off body, rewrites their task id, and forwards them
// on out. It closes body and out when done.
func (d *Dispatcher) pipeSSE(ctx context.Context, body io.ReadCloser, out chan<- StreamEvent, hubTaskID, upstreamID string) {
	defer body.Close()
	defer close(out)

	scanner := bufio.NewScanner(body)
	// SSE payloads can be big; give the scanner a generous buffer.
	buf := make([]byte, 0, 1<<20)
	scanner.Buffer(buf, 8<<20)

	var mappedUpstreamTask string
	var payload strings.Builder
	var sawTerminal bool

	send := func(evt StreamEvent) bool {
		select {
		case <-ctx.Done():
			return false
		case out <- evt:
			return true
		}
	}

	sendPayload := func() bool {
		raw := strings.TrimRight(payload.String(), "\n")
		payload.Reset()
		if raw == "" {
			return true
		}
		rewritten, state, upstreamTaskID, err := rewriteEventPayload([]byte(raw), hubTaskID)
		if err != nil {
			// Non-JSON or unexpected payload: forward as-is; don't crash.
			return send(StreamEvent{Data: []byte(raw)})
		}
		// First payload carries the upstream task id — persist the mapping.
		if mappedUpstreamTask == "" && upstreamTaskID != "" {
			mappedUpstreamTask = upstreamTaskID
			_ = d.Store.MapTaskID(ctx, hubTaskID, upstreamID, upstreamTaskID)
		}
		if state != "" {
			_ = d.Store.UpdateTaskSnapshot(ctx, hubTaskID, state, nil)
		}
		final := state.IsTerminal()
		if final {
			sawTerminal = true
		}
		return send(StreamEvent{Data: rewritten, Final: final})
	}

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return
		}
		line := scanner.Text()
		if line == "" {
			if !sendPayload() {
				return
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			// Preserve exact SSE `data:` payload minus the prefix (per spec:
			// trim the SINGLE leading space if present).
			d := strings.TrimPrefix(line, "data:")
			d = strings.TrimPrefix(d, " ")
			payload.WriteString(d)
			payload.WriteString("\n")
		}
		// Non-data lines (event:, id:, retry:) are intentionally dropped —
		// we forward payload-only, keeping the wire minimal.
	}
	// Any pending payload without a trailing blank line.
	_ = sendPayload()

	// Synthesize a terminal event on ANY abnormal end: either the scanner
	// errored (real network problem) OR EOF arrived without a terminal state
	// (upstream closed the stream cleanly but never signalled completion).
	// Either way the spec requires a clean terminal event, not a silent close.
	if !sawTerminal {
		reason := "upstream closed stream without terminal event"
		if err := scanner.Err(); err != nil {
			reason = "upstream disconnected: " + err.Error()
		}
		synth, _ := json.Marshal(a2a.TaskStatusUpdateEvent{
			TaskID: hubTaskID,
			Status: a2a.TaskStatus{
				State: a2a.TaskStateFailed,
				Message: &a2a.Message{
					Role:  a2a.RoleAgent,
					Parts: []a2a.Part{{Text: reason}},
				},
			},
			Final: true,
		})
		_ = d.Store.UpdateTaskSnapshot(ctx, hubTaskID, a2a.TaskStateFailed, nil)
		_ = send(StreamEvent{Data: synth, Err: scanner.Err(), Final: true})
	}
	_ = d.Store.WriteAudit(context.Background(), store.AuditEntry{
		HubTaskID: hubTaskID, UpstreamID: upstreamID, Event: store.EventStreamEnd,
	})
	slog.Debug("stream ended",
		"hub_task_id", hubTaskID,
		"upstream_id", upstreamID,
		"saw_terminal", sawTerminal,
	)
}

// rewriteEventPayload takes a raw SSE data payload (JSON) and replaces the
// upstream task id with hubTaskID. It also returns the parsed state (if it
// looks like a TaskStatusUpdateEvent) and the original upstream task id (if
// present).
func rewriteEventPayload(raw []byte, hubTaskID string) ([]byte, a2a.TaskState, string, error) {
	// Try both event shapes: TaskStatusUpdateEvent and TaskArtifactUpdateEvent
	// share the {id, contextId} fields. We only need to rewrite `id`.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, "", "", err
	}
	upstreamTaskID := unquote(envelope["id"])
	envelope["id"] = mustQuote(hubTaskID)

	var state a2a.TaskState
	if s, ok := envelope["status"]; ok {
		var st a2a.TaskStatus
		if err := json.Unmarshal(s, &st); err == nil {
			state = st.State
		}
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return nil, "", "", err
	}
	return out, state, upstreamTaskID, nil
}

func unquote(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func mustQuote(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
