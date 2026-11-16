// Package api is harness-wiki's HTTP surface: six endpoints over the service, in the standard
// library's ServeMux (Go 1.22 patterns) — no router dependency for six routes. It only translates:
// a request body becomes a service call, and the service's typed errors become status codes.
//
//	GET    /api/wiki                  list every wiki with its live status
//	POST   /api/wiki                  create a wiki                              → 201
//	DELETE /api/wiki/{slug}           delete a wiki
//	POST   /api/wiki/{slug}/ingest    ingest sources, in the background           → 202
//	POST   /api/wiki/{slug}/lint      health-check and repair, in the background  → 202
//	POST   /api/wiki/{slug}/query     ask a question, synchronously (no lock)     → 200
//
// A background job is followed through GET /api/wiki and the wiki's status. Errors use one shape,
// {"error": {"message": "...", "details": {...}}}, and the status code comes from what the error
// IS (the wiki package's typed errors), never from its wording.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gurcuff91/harness-wiki/service"
	"github.com/gurcuff91/harness-wiki/wiki"
)

// RequestLimit bounds a request body: inline sources can be large, but not unbounded.
const RequestLimit = 64 << 20 // 64 MB

// QueryMaxTimeout caps the timeout a client may ask of a query. The server sets no write timeout,
// so a query holds its connection open until it answers or times out. A variable so tests can
// shrink it.
var QueryMaxTimeout = 1800 * time.Second

// New returns the HTTP handler for a service.
func New(svc *service.Service) http.Handler {
	h := &handler{svc: svc}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/wiki", h.list)
	mux.HandleFunc("POST /api/wiki", h.create)
	mux.HandleFunc("DELETE /api/wiki/{slug}", h.delete)
	mux.HandleFunc("POST /api/wiki/{slug}/ingest", h.ingest)
	mux.HandleFunc("POST /api/wiki/{slug}/lint", h.lint)
	mux.HandleFunc("POST /api/wiki/{slug}/query", h.query)
	mux.HandleFunc("GET /api/docs/openapi.json", h.openapi)
	mux.HandleFunc("GET /api/health", h.health)
	return mux
}

type handler struct{ svc *service.Service }

// ── requests ────────────────────────────────────────────────────────────────

type createRequest struct {
	Slug        string `json:"slug"`
	Description string `json:"description"`
	Model       string `json:"model"`
}

type ingestRequest struct {
	Sources  []string    `json:"sources"`
	Files    []wiki.File `json:"files"`
	Guidance string      `json:"guidance"`
	Model    string      `json:"model"`
}

type lintRequest struct {
	Guidance string `json:"guidance"`
	Model    string `json:"model"`
}

type queryRequest struct {
	Question string `json:"question"`
	// Timeout is in seconds; 0 = the default. The server caps it.
	Timeout int    `json:"timeout"`
	Model   string `json:"model"`
}

// ── handlers ────────────────────────────────────────────────────────────────

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.List()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if !decode(w, r, &req) {
		return
	}
	c, err := h.svc.Create(r.Context(), req.Slug, req.Description, req.Model)
	if err != nil {
		fail(w, err)
		return
	}
	// A failed schema adaptation still leaves a usable wiki, so it is a 201 either way.
	writeJSON(w, http.StatusCreated, c.Info)
}

func (h *handler) delete(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Delete(r.PathValue("slug")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": map[string]string{"code": "deleted"}})
}

func (h *handler) ingest(w http.ResponseWriter, r *http.Request) {
	var req ingestRequest
	if !decode(w, r, &req) {
		return
	}
	slug := r.PathValue("slug")
	acc, err := h.svc.Ingest(slug, wiki.Sources{Paths: req.Sources, Files: req.Files}, req.Guidance, req.Model)
	if err != nil {
		fail(w, err)
		return
	}
	body := map[string]any{"slug": slug, "ingested": nonNil(acc.Ingest.Ingested)}
	if len(acc.Ingest.AlreadyIngested) > 0 {
		body["already_ingested"] = acc.Ingest.AlreadyIngested
	}
	if len(acc.Ingest.Skipped) > 0 {
		body["skipped"] = acc.Ingest.Skipped
	}
	h.addStatus(body, slug)
	writeJSON(w, http.StatusAccepted, body)
}

func (h *handler) lint(w http.ResponseWriter, r *http.Request) {
	var req lintRequest
	if !decode(w, r, &req) {
		return
	}
	slug := r.PathValue("slug")
	if _, err := h.svc.Lint(slug, req.Guidance, req.Model); err != nil {
		fail(w, err)
		return
	}
	body := map[string]any{"slug": slug}
	h.addStatus(body, slug)
	writeJSON(w, http.StatusAccepted, body)
}

func (h *handler) query(w http.ResponseWriter, r *http.Request) {
	var req queryRequest
	if !decode(w, r, &req) {
		return
	}
	timeout := time.Duration(req.Timeout) * time.Second
	if timeout > QueryMaxTimeout {
		timeout = QueryMaxTimeout
	}
	slug := r.PathValue("slug")
	answer, err := h.svc.Query(r.Context(), slug, req.Question, timeout, req.Model)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"slug": slug, "answer": answer})
}

// addStatus adds the wiki's current status to a background job's 202, so the client sees "busy"
// at once and knows to follow the job through GET /api/wiki. Nothing about the job's own
// bookkeeping — a temp file, say — is passed on: it would mean nothing to a remote client.
func (h *handler) addStatus(body map[string]any, slug string) {
	if info, err := h.svc.Store.Get(slug); err == nil {
		body["status"] = info.Status
		if info.Op != "" {
			body["op"] = info.Op
		}
	}
}

// ── plumbing ────────────────────────────────────────────────────────────────

// decode reads the JSON body into v. An empty body is fine (v keeps its zero value), so an
// endpoint whose fields are all optional — lint — can be called with no body at all.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, RequestLimit)
	err := json.NewDecoder(r.Body).Decode(v)
	switch {
	case err == nil, errors.Is(err, io.EOF):
		return true
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body is too large", nil)
		return false
	}
	writeError(w, http.StatusBadRequest, "invalid body: "+err.Error(), nil)
	return false
}

// fail writes the response for a failed operation. The status comes from what the error IS.
func fail(w http.ResponseWriter, err error) {
	var busy *wiki.ErrBusy
	var conflict *wiki.ConflictError
	var timeout *service.TimeoutError
	switch {
	case errors.As(err, &busy):
		details := map[string]any{"slug": busy.Slug, "op": busy.Op}
		if !busy.Since.IsZero() {
			details["since"] = busy.Since.UTC().Format(time.RFC3339)
			details["elapsed_seconds"] = int(time.Since(busy.Since).Seconds())
		}
		writeError(w, http.StatusConflict, err.Error(), details)
	case errors.As(err, &conflict):
		writeError(w, http.StatusConflict, err.Error(), map[string]any{"conflicts": conflict.Paths})
	case errors.Is(err, wiki.ErrExists):
		writeError(w, http.StatusConflict, err.Error(), nil)
	case errors.Is(err, wiki.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error(), nil)
	case errors.Is(err, wiki.ErrInvalidSlug), errors.Is(err, wiki.ErrInvalidPath), errors.Is(err, wiki.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error(), nil)
	case errors.As(err, &timeout):
		writeError(w, http.StatusGatewayTimeout, err.Error(), nil)
	case errors.Is(err, context.Canceled):
		// the client went away; nobody is listening, but keep the log line honest
		writeError(w, 499, "client closed the request", nil)
	default:
		writeError(w, http.StatusInternalServerError, err.Error(), nil)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string, details map[string]any) {
	e := map[string]any{"message": message}
	if len(details) > 0 {
		e["details"] = details
	}
	writeJSON(w, status, map[string]any{"error": e})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
