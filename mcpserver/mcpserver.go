// Package mcpserver exposes the wiki service as an MCP (Model Context Protocol) server over HTTP —
// the "Streamable HTTP" transport — so a harness (or any MCP client) can use the wikis as tools by
// pointing at one URL:
//
//	"mcp": { "wiki": { "url": "http://127.0.0.1:8787/mcp", "timeout": 600000 } }
//
// It is a thin protocol layer over the same service the REST API uses, so the rules of every
// operation — the lock, the all-or-nothing ingest, queries that take no lock — are the same and are
// not repeated here.
//
// The server is stateless: it assigns no Mcp-Session-Id and keeps nothing between requests, so a
// restart loses nothing and any number of clients can connect. Every JSON-RPC message is a POST to
// the one endpoint, and is answered with a single JSON object (or 202 with no body for a
// notification). Server-initiated messages and SSE streams are not used: none of the six tools
// streams anything, and the harness client reads a plain JSON reply.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gurcuff91/harness-wiki/internal/version"
	"github.com/gurcuff91/harness-wiki/service"
)

// ProtocolVersion is the MCP revision this server speaks. It is the one harness's client sends;
// the tools primitive has not changed across revisions, and a client asking for another is told
// this one (the spec's negotiation: the server answers with the version it supports).
const ProtocolVersion = "2024-11-05"

// RequestLimit bounds a request body. An ingest can carry inline files, so it is generous.
const RequestLimit = 64 << 20 // 64 MB

// JSON-RPC 2.0 error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// Handler serves MCP on one endpoint.
type Handler struct {
	svc   *service.Service
	tools []tool
	index map[string]*tool
}

// New returns the MCP handler for a service.
func New(svc *service.Service) *Handler {
	h := &Handler{svc: svc, index: map[string]*tool{}}
	h.tools = buildTools(svc)
	for i := range h.tools {
		h.index[h.tools[i].Name] = &h.tools[i]
	}
	return h
}

// ServeHTTP implements http.Handler: POST carries one JSON-RPC message.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// GET would open a server→client SSE stream and DELETE would end a session; this server
		// does neither, and the spec lets it say so with 405.
		w.Header().Set("Allow", "POST")
		http.Error(w, "this MCP endpoint accepts POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, RequestLimit))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeRPC(w, http.StatusRequestEntityTooLarge, errorResponse(nil, codeInvalidRequest, "request body is too large"))
			return
		}
		writeRPC(w, http.StatusBadRequest, errorResponse(nil, codeParseError, "read: "+err.Error()))
		return
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		writeRPC(w, http.StatusBadRequest, errorResponse(nil, codeInvalidRequest, "empty request"))
		return
	}
	// A JSON array is a batch (an MCP revision allows it); a client of this server sends single
	// messages, but answering a batch correctly costs little.
	if strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		h.serveBatch(w, r, body)
		return
	}
	resp, isNotification := h.handleOne(r.Context(), body)
	if isNotification {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeRPC(w, http.StatusOK, resp)
}

func (h *Handler) serveBatch(w http.ResponseWriter, r *http.Request, body []byte) {
	var raws []json.RawMessage
	if err := json.Unmarshal(body, &raws); err != nil {
		writeRPC(w, http.StatusBadRequest, errorResponse(nil, codeParseError, "parse error: "+err.Error()))
		return
	}
	if len(raws) == 0 {
		writeRPC(w, http.StatusBadRequest, errorResponse(nil, codeInvalidRequest, "empty batch"))
		return
	}
	out := make([]response, 0, len(raws))
	for _, raw := range raws {
		if resp, isNotification := h.handleOne(r.Context(), raw); !isNotification {
			out = append(out, resp)
		}
	}
	if len(out) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeRPC(w, http.StatusOK, out)
}

// ── JSON-RPC ────────────────────────────────────────────────────────────────

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent for a notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func errorResponse(id json.RawMessage, code int, msg string) response {
	if id == nil {
		id = json.RawMessage("null")
	}
	return response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

func okResponse(id json.RawMessage, result any) response {
	return response{JSONRPC: "2.0", ID: id, Result: result}
}

func writeRPC(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// handleOne dispatches one JSON-RPC message. isNotification is true for a message with no id: it
// is never answered (the caller sends 202 with no body).
func (h *Handler) handleOne(ctx context.Context, raw []byte) (resp response, isNotification bool) {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		if json.Valid(raw) {
			// Well-formed JSON that is not a request object (a number, a string…).
			return errorResponse(nil, codeInvalidRequest, "invalid request: a message must be a JSON object"), false
		}
		return errorResponse(nil, codeParseError, "parse error: "+err.Error()), false
	}
	hasID := len(req.ID) > 0 && string(req.ID) != "null"
	if req.JSONRPC != "2.0" || req.Method == "" {
		return errorResponse(req.ID, codeInvalidRequest, `invalid request: "jsonrpc" must be "2.0" and "method" is required`), false
	}
	if !hasID {
		// A notification (notifications/initialized, notifications/cancelled, …): nothing to answer.
		return response{}, true
	}
	switch req.Method {
	case "initialize":
		return h.initialize(req), false
	case "ping":
		return okResponse(req.ID, struct{}{}), false
	case "tools/list":
		return h.toolsList(req), false
	case "tools/call":
		return h.toolsCall(ctx, req), false
	default:
		return errorResponse(req.ID, codeMethodNotFound, "method not found: "+req.Method), false
	}
}

func (h *Handler) initialize(req request) response {
	return okResponse(req.ID, map[string]any{
		"protocolVersion": ProtocolVersion,
		"serverInfo":      map[string]string{"name": "harness-wiki", "version": version.Version},
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"instructions": "LLM-maintained wikis: persistent, interlinked markdown knowledge bases that a model builds and keeps current from the sources you ingest. " +
			"WikiList shows every wiki and its status (ready, busy, incomplete); WikiIngest and WikiLint run in the background — call WikiList later to see whether they finished; WikiQuery answers a question and takes no lock.",
	})
}

func (h *Handler) toolsList(req request) response {
	type entry struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	entries := make([]entry, len(h.tools))
	for i, t := range h.tools {
		entries[i] = entry{Name: t.Name, Description: t.Description, InputSchema: t.Schema}
	}
	return okResponse(req.ID, map[string]any{"tools": entries})
}

func (h *Handler) toolsCall(ctx context.Context, req request) response {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errorResponse(req.ID, codeInvalidParams, "invalid params: "+err.Error())
	}
	t, ok := h.index[p.Name]
	if !ok {
		return errorResponse(req.ID, codeInvalidParams, "unknown tool: "+p.Name)
	}
	if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
		p.Arguments = json.RawMessage("{}")
	}
	out, err := t.Execute(ctx, p.Arguments)
	if err != nil {
		// A tool failure is a RESULT with isError, not a JSON-RPC error: the client hands it back
		// to the model, which can read it and recover (retry later on "busy", fix a bad name…).
		return okResponse(req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": toolErrorText(err)}},
			"isError": true,
		})
	}
	return okResponse(req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": out}},
	})
}

// toolErrorText is the message the model reads for a failed tool call.
func toolErrorText(err error) string {
	return "error: " + err.Error()
}

// timeoutOf converts a "timeout" argument in seconds to a duration (0 = the service default). A
// model may send a fraction, or an absurd number: it is clamped before the conversion, which would
// otherwise overflow, and the service applies its own cap on top.
func timeoutOf(seconds float64) time.Duration {
	if seconds <= 0 {
		return 0
	}
	if max := service.MaxTimeout.Seconds(); seconds > max {
		seconds = max
	}
	return time.Duration(seconds * float64(time.Second))
}

// invalid builds an argument error. The service's own validation produces wiki.ErrInvalidInput; this
// is for what the tool layer itself checks (a missing required field).
func invalid(format string, a ...any) error { return fmt.Errorf("invalid input: "+format, a...) }
