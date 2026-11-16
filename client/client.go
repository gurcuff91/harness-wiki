// Package client is the typed Go client for harness-wiki's HTTP API. It speaks to a running
// `harness-wiki serve` and needs nothing else — in particular not the harness SDK.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one harness-wiki server.
type Client struct {
	base string
	http *http.Client
	ctx  context.Context
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient replaces the underlying *http.Client (for a proxy, TLS settings, a transport).
// Do not set a Timeout on it: a query legitimately holds its connection open for minutes — bound
// a call with WithContext, or pass a timeout to Query.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithContext binds every call to ctx.
func WithContext(ctx context.Context) Option { return func(c *Client) { c.ctx = ctx } }

// New returns a client for the server at addr: "host:port" or a full "http://host:port" URL.
func New(addr string, opts ...Option) *Client {
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	c := &Client{base: strings.TrimRight(addr, "/"), http: &http.Client{}, ctx: context.Background()}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Status is the answer of an action endpoint such as delete.
type Status struct {
	Code string `json:"code"`
}

// Error is a structured error from the server: its message plus optional details, and the HTTP
// status, so a caller can classify a failure (404 vs 409 vs 504) without matching on text.
type Error struct {
	Message string
	Details map[string]any
	Status  int
}

func (e *Error) Error() string {
	if len(e.Details) > 0 {
		if d, err := json.Marshal(e.Details); err == nil {
			return e.Message + ": " + string(d)
		}
	}
	return e.Message
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if resp.StatusCode >= 400 {
		var env struct {
			Error struct {
				Message string         `json:"message"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &env) == nil && env.Error.Message != "" {
			return &Error{Message: env.Error.Message, Details: env.Error.Details, Status: resp.StatusCode}
		}
		return &Error{Message: fmt.Sprintf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data))), Status: resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}

func decode[T any](c *Client, ctx context.Context, method, path string, body any) (T, error) {
	var out T
	err := c.do(ctx, method, path, body, &out)
	return out, err
}

func ptr[T any](v T, err error) (*T, error) {
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// WikiInfo is one wiki, as GET /api/wiki returns it. Status is "ready", "busy" (an operation
// is running: Op and Since say which and since when) or "incomplete" (an ingest or lint was
// interrupted and the wiki may be half updated — run a lint).
type WikiInfo struct {
	Slug        string    `json:"slug"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	Status      string    `json:"status"`
	Op          string    `json:"op,omitempty"`
	Since       time.Time `json:"since,omitzero"`
}

// WikiFile is an inline source for WikiIngest: stored as a file at raw/<Name>. Name may be a
// path ("ctx/data.md") to create folders.
type WikiFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// WikiSources is what WikiIngest adds to a wiki's raw/ folder. At least one of the two is needed.
type WikiSources struct {
	// Paths are on the SERVER, not on the caller's machine: a file, a glob ("**" matches
	// across directories) or a directory (copied whole, directory included).
	Paths []string `json:"sources,omitempty"`
	// Files are inline sources, each stored as a file.
	Files []WikiFile `json:"files,omitempty"`
	// Guidance optionally steers the maintainer for this ingest; it does not change the
	// wiki's rules.
	Guidance string `json:"guidance,omitempty"`
}

// WikiAccepted is the answer to WikiIngest and WikiLint: the job was started and keeps running
// in the background. There is no result report — follow it with WikiList and the wiki's
// Status ("busy" while it works, "ready" when it finished, "incomplete" if it was cut short).
type WikiAccepted struct {
	Slug   string `json:"slug"`
	Status string `json:"status,omitempty"`
	Op     string `json:"op,omitempty"`
	// Ingest only, paths relative to the wiki's raw/ folder:
	Ingested        []string `json:"ingested,omitempty"`
	AlreadyIngested []string `json:"already_ingested,omitempty"`
	Skipped         []string `json:"skipped,omitempty"`
}

// WikiList returns every wiki with its live status (GET /api/wiki). The list is never nil.
// A server whose agent has no wiki store answers 503 (see IsWikiUnavailable).
func (c *Client) WikiList() ([]WikiInfo, error) {
	list, err := decode[[]WikiInfo](c, c.ctx, "GET", "/api/wiki", nil)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []WikiInfo{}
	}
	return list, nil
}

// WikiCreate creates a wiki (POST /api/wiki). The server also has the wiki's maintainer adapt
// its schema to the description; if that fails the wiki still exists and is usable.
func (c *Client) WikiCreate(slug, description string) (*WikiInfo, error) {
	return ptr(decode[WikiInfo](c, c.ctx, "POST", "/api/wiki", map[string]string{"slug": slug, "description": description}))
}

// WikiDelete removes a wiki entirely (DELETE /api/wiki/{slug}). A busy wiki is refused (see
// IsWikiBusy); a missing one is a 404 *Error.
func (c *Client) WikiDelete(slug string) (*Status, error) {
	var env struct {
		Status Status `json:"status"`
	}
	if err := c.do(c.ctx, "DELETE", wikiPath(slug), nil, &env); err != nil {
		return nil, err
	}
	return &env.Status, nil
}

// WikiIngest copies the sources into the wiki's raw/ folder and starts its maintainer in the
// background (POST /api/wiki/{slug}/ingest). It returns as soon as the sources are copied.
// A file that already exists with different content refuses the whole ingest with a 409 whose
// Details["conflicts"] lists them; a busy wiki is refused too (see IsWikiBusy).
func (c *Client) WikiIngest(slug string, src WikiSources) (*WikiAccepted, error) {
	return ptr(decode[WikiAccepted](c, c.ctx, "POST", wikiPath(slug)+"/ingest", src))
}

// WikiLint starts a health check and repair of the wiki in the background (POST
// /api/wiki/{slug}/lint). guidance is optional.
func (c *Client) WikiLint(slug, guidance string) (*WikiAccepted, error) {
	var body any
	if guidance != "" {
		body = map[string]string{"guidance": guidance}
	}
	return ptr(decode[WikiAccepted](c, c.ctx, "POST", wikiPath(slug)+"/lint", body))
}

// WikiQuery asks a wiki a question and waits for the answer (POST /api/wiki/{slug}/query).
// A query takes no lock on the wiki: any number can run at once, even while an ingest or a lint is
// working on it (the maintainer may then see pages that are still being updated).
// timeout bounds both the maintainer on the server (in whole seconds; the server caps it) and
// this call; 0 uses the server's default of 300 s and waits as long as the connection allows.
// A timeout is a 504 *Error.
func (c *Client) WikiQuery(slug, question string, timeout time.Duration) (string, error) {
	body := map[string]any{"question": question}
	ctx := c.ctx
	if timeout > 0 {
		body["timeout"] = int((timeout + time.Second - 1) / time.Second)
		// A little slack over the server-side timeout, so the server's own 504 (a clean,
		// classified error) arrives before this call gives up on its own.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout+10*time.Second)
		defer cancel()
	}
	out, err := decode[struct {
		Answer string `json:"answer"`
	}](c, ctx, "POST", wikiPath(slug)+"/query", body)
	return out.Answer, err
}

func wikiPath(slug string) string { return "/api/wiki/" + url.PathEscape(slug) }

// IsWikiBusy reports whether err is the server refusing because another operation holds the
// wiki. The error's Details carry "slug", "op", "since" (RFC 3339) and "elapsed_seconds".
func IsWikiBusy(err error) bool {
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != http.StatusConflict {
		return false
	}
	_, hasOp := ae.Details["op"]
	_, hasSlug := ae.Details["slug"]
	return hasOp && hasSlug
}

// IsWikiConflict reports whether err is an ingest refused because files already exist in raw/
// with different content, and returns those paths (relative to raw/). Nothing was copied.
func IsWikiConflict(err error) (paths []string, ok bool) {
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != http.StatusConflict {
		return nil, false
	}
	raw, has := ae.Details["conflicts"].([]any)
	if !has {
		return nil, false
	}
	for _, p := range raw {
		if s, ok := p.(string); ok {
			paths = append(paths, s)
		}
	}
	return paths, true
}

// IsWikiUnavailable reports whether err is the server saying its agent has no wiki store (503).
func IsWikiUnavailable(err error) bool {
	var ae *Error
	return errors.As(err, &ae) && ae.Status == http.StatusServiceUnavailable
}
