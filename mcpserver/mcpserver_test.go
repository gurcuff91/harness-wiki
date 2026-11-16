package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	harnessmcp "github.com/gurcuff91/harness/mcp"

	"github.com/gurcuff91/harness-wiki/internal/version"
	"github.com/gurcuff91/harness-wiki/maintainer"
	"github.com/gurcuff91/harness-wiki/service"
	"github.com/gurcuff91/harness-wiki/wiki"
)

// ── fixtures ────────────────────────────────────────────────────────────────

// fakeAI answers a run, or holds it until released. It records what it was asked.
type fakeAI struct {
	mu      sync.Mutex
	runs    []maintainer.Run
	hold    map[maintainer.Op]chan struct{}
	reply   string
	fail    error
	started chan maintainer.Run
	// cancelled gets a run's op when its context ended while it was held.
	cancelled chan maintainer.Op
}

func newFakeAI() *fakeAI {
	return &fakeAI{reply: "the maintainer says: done", hold: map[maintainer.Op]chan struct{}{}, started: make(chan maintainer.Run, 64), cancelled: make(chan maintainer.Op, 8)}
}

func (f *fakeAI) Execute(ctx context.Context, run maintainer.Run) (string, error) {
	f.mu.Lock()
	f.runs = append(f.runs, run)
	gate, reply, fail := f.hold[run.Op], f.reply, f.fail
	f.mu.Unlock()
	f.started <- run
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			f.cancelled <- run.Op
			return "partial", ctx.Err()
		}
	}
	return reply, fail
}

func (f *fakeAI) setHold(op maintainer.Op, ch chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hold[op] = ch
}

func (f *fakeAI) setFail(err error) { f.mu.Lock(); f.fail = err; f.mu.Unlock() }

func (f *fakeAI) last() maintainer.Run {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[len(f.runs)-1]
}

func (f *fakeAI) waitStarted(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-f.started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d runs started", i, n)
		}
	}
}

type env struct {
	t    *testing.T
	srv  *httptest.Server
	svc  *service.Service
	ai   *fakeAI
	st   *wiki.FSStore
	root string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := filepath.Join(t.TempDir(), "harness-wiki")
	st, err := wiki.OpenFS(root)
	if err != nil {
		t.Fatal(err)
	}
	ai := newFakeAI()
	svc := service.New(st, ai)
	srv := httptest.NewServer(New(svc))
	t.Cleanup(func() { srv.Close(); svc.Close(2 * time.Second); st.Close() })
	return &env{t: t, srv: srv, svc: svc, ai: ai, st: st, root: root}
}

// post sends a raw body and returns the status and the raw reply.
func (e *env) post(body string) (int, string) {
	e.t.Helper()
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Post(e.srv.URL, "application/json", strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

type rpcReply struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// rpc sends one request and decodes its reply.
func (e *env) rpc(method string, params any) rpcReply {
	e.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
	code, raw := e.post(string(b))
	if code != 200 {
		e.t.Fatalf("%s: status %d: %s", method, code, raw)
	}
	var r rpcReply
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		e.t.Fatalf("%s: bad reply %q: %v", method, raw, err)
	}
	if string(r.ID) != "7" {
		e.t.Fatalf("%s: reply id %s, want the request's 7", method, r.ID)
	}
	return r
}

type callOut struct {
	Text    string
	IsError bool
}

// call runs a tool and returns its text and isError flag; a JSON-RPC error fails the test.
func (e *env) call(name string, args any) callOut {
	e.t.Helper()
	r := e.rpc("tools/call", map[string]any{"name": name, "arguments": args})
	if r.Error != nil {
		e.t.Fatalf("%s: unexpected JSON-RPC error %+v", name, r.Error)
	}
	var res struct {
		Content []struct{ Type, Text string }
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil || len(res.Content) != 1 || res.Content[0].Type != "text" {
		e.t.Fatalf("%s: malformed result %s (%v)", name, r.Result, err)
	}
	return callOut{res.Content[0].Text, res.IsError}
}

func (e *env) mustCall(name string, args any) string {
	e.t.Helper()
	out := e.call(name, args)
	if out.IsError {
		e.t.Fatalf("%s failed: %s", name, out.Text)
	}
	return out.Text
}

func (e *env) create(slug string) {
	e.t.Helper()
	e.mustCall("WikiCreate", map[string]any{"slug": slug, "description": "d"})
	e.ai.waitStarted(e.t, 1)
}

func (e *env) waitStatus(slug string, want wiki.Status) {
	e.t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		info, err := e.st.Get(slug)
		if err == nil && info.Status == want {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s never became %s (last %+v, %v)", slug, want, info, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func contains(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%q does not mention %q", got, w)
		}
	}
}

// ── protocol ────────────────────────────────────────────────────────────────

func TestInitializeAnswersWithTheServerIdentityAndOnlyTheToolsCapability(t *testing.T) {
	e := newEnv(t)
	r := e.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "x", "version": "1"}})
	var res struct {
		ProtocolVersion string         `json:"protocolVersion"`
		ServerInfo      map[string]any `json:"serverInfo"`
		Capabilities    map[string]any `json:"capabilities"`
		Instructions    string         `json:"instructions"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	// The literal, not the constant: the revision is part of the contract with the clients that
	// pin it (harness's own sends 2024-11-05), so changing it must be a deliberate edit here too.
	if res.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocolVersion %q: a client asking for another revision is told the one this server speaks (2024-11-05)", res.ProtocolVersion)
	}
	if res.ServerInfo["name"] != "harness-wiki" || res.ServerInfo["version"] != version.Version {
		t.Errorf("serverInfo %v", res.ServerInfo)
	}
	if _, ok := res.Capabilities["tools"]; !ok || len(res.Capabilities) != 1 {
		t.Errorf("capabilities %v, want only tools", res.Capabilities)
	}
	contains(t, res.Instructions, "WikiList")
}

func TestPingAnswersWithAnEmptyObject(t *testing.T) {
	e := newEnv(t)
	if r := e.rpc("ping", nil); r.Error != nil || string(r.Result) != "{}" {
		t.Fatalf("ping: %+v, want an empty result object", r)
	}
}

func TestToolsListAdvertisesTheSixToolsWithObjectSchemas(t *testing.T) {
	e := newEnv(t)
	var res struct {
		Tools []struct {
			Name        string
			Description string
			InputSchema struct {
				Type       string
				Properties map[string]json.RawMessage
				Required   []string
			}
		}
	}
	if err := json.Unmarshal(e.rpc("tools/list", nil).Result, &res); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"WikiList":   nil,
		"WikiCreate": {"slug", "description"},
		"WikiDelete": {"slug"},
		"WikiIngest": {"slug"},
		"WikiQuery":  {"slug", "question"},
		"WikiLint":   {"slug"},
	}
	if len(res.Tools) != len(want) {
		t.Fatalf("%d tools, want %d", len(res.Tools), len(want))
	}
	for _, tl := range res.Tools {
		req, ok := want[tl.Name]
		if !ok {
			t.Errorf("unexpected tool %q", tl.Name)
			continue
		}
		if tl.Description == "" || tl.InputSchema.Type != "object" || tl.InputSchema.Properties == nil {
			t.Errorf("%s: description %q, schema %+v: every tool needs a description and an object schema with properties", tl.Name, tl.Description, tl.InputSchema)
		}
		if strings.Join(tl.InputSchema.Required, ",") != strings.Join(req, ",") {
			t.Errorf("%s requires %v, want %v", tl.Name, tl.InputSchema.Required, req)
		}
		for _, f := range req {
			if _, ok := tl.InputSchema.Properties[f]; !ok {
				t.Errorf("%s requires %q but does not declare it", tl.Name, f)
			}
		}
	}
}

func TestEveryDeclaredPropertyHasADescription(t *testing.T) {
	// A model decides how to fill a field from its description, so none may be left bare.
	for _, tl := range buildTools(nil) {
		var s struct{ Properties map[string]prop }
		if err := json.Unmarshal(tl.Schema, &s); err != nil {
			t.Fatal(err)
		}
		for name, p := range s.Properties {
			if p.Description == "" || p.Type == "" {
				t.Errorf("%s.%s has no type/description", tl.Name, name)
			}
			if p.Type == "array" && (p.Items == nil || p.Items.Type == "") {
				t.Errorf("%s.%s is an array with no item type", tl.Name, name)
			}
		}
	}
}

func TestANotificationIsAcceptedWithNoBody(t *testing.T) {
	e := newEnv(t)
	code, raw := e.post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if code != http.StatusAccepted || raw != "" {
		t.Fatalf("notification: %d %q, want 202 and no body", code, raw)
	}
	// An explicit null id is still not a request.
	code, raw = e.post(`{"jsonrpc":"2.0","id":null,"method":"notifications/cancelled"}`)
	if code != http.StatusAccepted || raw != "" {
		t.Fatalf("null-id notification: %d %q", code, raw)
	}
}

func TestANotificationNeverRunsAnything(t *testing.T) {
	// "tools/call" without an id is a notification: it must not execute.
	e := newEnv(t)
	code, _ := e.post(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"WikiCreate","arguments":{"slug":"sneaky","description":"d"}}}`)
	if code != http.StatusAccepted {
		t.Fatalf("status %d", code)
	}
	if _, err := e.st.Get("sneaky"); err == nil {
		t.Fatal("a notification created a wiki")
	}
}

func TestAStringIDIsEchoedBack(t *testing.T) {
	e := newEnv(t)
	code, raw := e.post(`{"jsonrpc":"2.0","id":"abc-1","method":"ping"}`)
	if code != 200 || !strings.Contains(raw, `"id":"abc-1"`) {
		t.Fatalf("%d %s", code, raw)
	}
}

func TestProtocolErrors(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		name, body string
		status     int
		code       int
	}{
		{"unknown method", `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, 200, codeMethodNotFound},
		{"not json", `{nope`, 200, codeParseError},
		{"wrong version", `{"jsonrpc":"1.0","id":1,"method":"ping"}`, 200, codeInvalidRequest},
		{"no method", `{"jsonrpc":"2.0","id":1}`, 200, codeInvalidRequest},
		{"unknown tool", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"WikiNope"}}`, 200, codeInvalidParams},
		{"bad params", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"x"}`, 200, codeInvalidParams},
		{"no params", `{"jsonrpc":"2.0","id":1,"method":"tools/call"}`, 200, codeInvalidParams},
	} {
		code, raw := e.post(c.body)
		var r rpcReply
		json.Unmarshal([]byte(raw), &r)
		if code != c.status || r.Error == nil || r.Error.Code != c.code {
			t.Errorf("%s: %d %s, want status %d and error %d", c.name, code, raw, c.status, c.code)
		}
	}
	// A parse error and an invalid request are answered with id null, per JSON-RPC.
	_, raw := e.post(`{nope`)
	contains(t, raw, `"id":null`)
}

func TestEmptyAndOversizedBodies(t *testing.T) {
	e := newEnv(t)
	if code, raw := e.post("  \n"); code != http.StatusBadRequest || !strings.Contains(raw, "empty request") {
		t.Errorf("empty body: %d %s", code, raw)
	}
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` + strings.Repeat("x", RequestLimit) + `"}}`
	code, raw := e.post(big)
	if code != http.StatusRequestEntityTooLarge || !strings.Contains(raw, "too large") {
		n := len(raw)
		if n > 120 {
			n = 120
		}
		t.Errorf("oversized body: %d %s, want 413 and \"too large\"", code, raw[:n])
	}
}

func TestOnlyPOSTIsAccepted(t *testing.T) {
	e := newEnv(t)
	for _, m := range []string{"GET", "DELETE", "PUT", "PATCH"} {
		req, _ := http.NewRequest(m, e.srv.URL, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "POST" {
			t.Errorf("%s: %d Allow=%q, want 405 and Allow: POST", m, resp.StatusCode, resp.Header.Get("Allow"))
		}
	}
}

func TestTheServerIsStatelessAndAssignsNoSession(t *testing.T) {
	e := newEnv(t)
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	resp, err := http.Post(e.srv.URL, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if h := resp.Header.Get("Mcp-Session-Id"); h != "" {
		t.Errorf("Mcp-Session-Id %q: a stateless server must not assign one", h)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type %q, want application/json (no SSE)", ct)
	}
}

func TestBatches(t *testing.T) {
	e := newEnv(t)
	code, raw := e.post(`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":2,"method":"nope"}]`)
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	var out []rpcReply
	if err := json.Unmarshal([]byte(raw), &out); err != nil || len(out) != 2 {
		t.Fatalf("batch reply %s (%v): want 2 answers, the notification gets none", raw, err)
	}
	if string(out[0].ID) != "1" || out[0].Error != nil || string(out[1].ID) != "2" || out[1].Error == nil || out[1].Error.Code != codeMethodNotFound {
		t.Errorf("batch answers %+v", out)
	}
	if code, raw := e.post(`[{"jsonrpc":"2.0","method":"notifications/initialized"}]`); code != http.StatusAccepted || raw != "" {
		t.Errorf("a batch of only notifications: %d %q, want 202", code, raw)
	}
	if code, raw := e.post(`[]`); code != http.StatusBadRequest || !strings.Contains(raw, "empty batch") {
		t.Errorf("empty batch: %d %s", code, raw)
	}
	if code, raw := e.post(`[1,2]`); code != 200 || !strings.Contains(raw, `"code":-32600`) {
		t.Errorf("batch of non-objects: %d %s", code, raw)
	}
}

// ── tools ───────────────────────────────────────────────────────────────────

func TestListIsAnEmptyJSONArrayAtFirstAndShowsEachWiki(t *testing.T) {
	e := newEnv(t)
	if got := e.mustCall("WikiList", nil); strings.TrimSpace(got) != "[]" {
		t.Fatalf("empty list: %q", got)
	}
	if _, err := os.Stat(e.root); err == nil {
		t.Fatal("listing created the store root")
	}
	e.create("alpha")
	var list []map[string]any
	if err := json.Unmarshal([]byte(e.mustCall("WikiList", map[string]any{})), &list); err != nil || len(list) != 1 || list[0]["slug"] != "alpha" || list[0]["status"] != "ready" {
		t.Fatalf("list %v (%v)", list, err)
	}
}

func TestCreateMakesTheWikiAndReportsTheAdaptation(t *testing.T) {
	e := newEnv(t)
	got := e.mustCall("WikiCreate", map[string]any{"slug": "rust", "description": "Rust notes", "model": "p/m"})
	contains(t, got, `"rust"`, "adapted")
	if strings.Contains(got, "NOT") {
		t.Errorf("a successful adaptation was reported as failed: %q", got)
	}
	run := e.ai.last()
	if run.Op != maintainer.OpInit || run.Slug != "rust" || run.Model != "p/m" {
		t.Errorf("the adaptation ran as %+v, want init on rust with model p/m", run)
	}
	for _, f := range []string{"wiki.json", "AGENTS.md", "wiki/index.md"} {
		if _, err := os.Stat(filepath.Join(e.root, "rust", filepath.FromSlash(f))); err != nil {
			t.Errorf("create did not make %s", f)
		}
	}
}

func TestCreateWithAFailedAdaptationStillSucceedsAndSaysWhy(t *testing.T) {
	e := newEnv(t)
	e.ai.setFail(context.DeadlineExceeded)
	out := e.call("WikiCreate", map[string]any{"slug": "rust", "description": "d"})
	if out.IsError {
		t.Fatalf("the wiki exists, so this is not an error: %s", out.Text)
	}
	contains(t, out.Text, "created", "NOT", "base rules", context.DeadlineExceeded.Error())
	if _, err := e.st.Get("rust"); err != nil {
		t.Errorf("the wiki must exist: %v", err)
	}
}

func TestCreateErrors(t *testing.T) {
	e := newEnv(t)
	e.create("taken")
	for _, c := range []struct {
		name string
		args any
		want []string
	}{
		{"no slug", map[string]any{"description": "d"}, []string{"slug", "required"}},
		{"blank slug", map[string]any{"slug": "  ", "description": "d"}, []string{"slug", "required"}},
		{"no description", map[string]any{"slug": "ok"}, []string{"description", "required"}},
		{"invalid slug", map[string]any{"slug": "Bad Slug", "description": "d"}, []string{"invalid wiki slug"}},
		{"path-like slug", map[string]any{"slug": "../x", "description": "d"}, []string{"invalid wiki slug"}},
		{"already exists", map[string]any{"slug": "taken", "description": "d"}, []string{"already exists"}},
		{"wrong type", map[string]any{"slug": 5, "description": "d"}, []string{"invalid input", "arguments"}},
	} {
		out := e.call("WikiCreate", c.args)
		if !out.IsError {
			t.Errorf("%s: not reported as an error: %q", c.name, out.Text)
			continue
		}
		contains(t, out.Text, c.want...)
		if !strings.HasPrefix(out.Text, "error: ") {
			t.Errorf("%s: %q does not start with %q", c.name, out.Text, "error: ")
		}
	}
	if len(e.ai.runs) != 1 {
		t.Errorf("a refused create ran the maintainer: %d runs", len(e.ai.runs))
	}
}

func TestDelete(t *testing.T) {
	e := newEnv(t)
	e.create("gone")
	contains(t, e.mustCall("WikiDelete", map[string]any{"slug": "gone"}), "deleted", `"gone"`)
	if _, err := e.st.Get("gone"); err == nil {
		t.Fatal("the wiki is still there")
	}
	for _, c := range []struct {
		args any
		want string
	}{
		{map[string]any{"slug": "gone"}, "not found"},
		{map[string]any{}, "required"},
		{map[string]any{"slug": "../x"}, "invalid wiki slug"},
	} {
		if out := e.call("WikiDelete", c.args); !out.IsError || !strings.Contains(out.Text, c.want) {
			t.Errorf("delete %v: %+v, want an error mentioning %q", c.args, out, c.want)
		}
	}
}

func TestIngestReturnsAtOnceWhileTheMaintainerWorksInTheBackground(t *testing.T) {
	e := newEnv(t)
	e.create("w")
	gate := make(chan struct{})
	e.ai.setHold(maintainer.OpIngest, gate)

	start := time.Now()
	got := e.mustCall("WikiIngest", map[string]any{
		"slug":     "w",
		"files":    []map[string]string{{"name": "a.md", "content": "alpha"}, {"name": "sub/b.md", "content": "beta"}},
		"guidance": "focus on b",
		"model":    "p/m",
	})
	if time.Since(start) > 2*time.Second {
		t.Fatal("ingest waited for the maintainer")
	}
	contains(t, got, "Ingest started", "ingested:", "a.md", "sub/b.md", "Status: busy (ingest)", "WikiList")
	e.waitStatus("w", wiki.StatusBusy)
	if b, err := os.ReadFile(filepath.Join(e.root, "w", "raw", "sub", "b.md")); err != nil || string(b) != "beta" {
		t.Errorf("the inline file was not written under raw/: %q %v", b, err)
	}
	e.ai.waitStarted(t, 1)
	if run := e.ai.last(); run.Op != maintainer.OpIngest || run.Model != "p/m" || !strings.Contains(run.Prompt, "focus on b") {
		t.Errorf("the maintainer ran as %+v, want ingest on p/m with the guidance in its prompt", run)
	}
	close(gate)
	e.waitStatus("w", wiki.StatusReady)
}

func TestIngestFromServerPathsAndAlreadyIngestedAreReported(t *testing.T) {
	e := newEnv(t)
	e.create("w")
	src := filepath.Join(t.TempDir(), "doc.md")
	os.WriteFile(src, []byte("same"), 0o644)
	got := e.mustCall("WikiIngest", map[string]any{"slug": "w", "sources": []string{src}})
	contains(t, got, "ingested: doc.md")
	e.ai.waitStarted(t, 1)
	e.waitStatus("w", wiki.StatusReady)

	got = e.mustCall("WikiIngest", map[string]any{"slug": "w", "sources": []string{src}})
	contains(t, got, "already ingested (not repeated): doc.md")
	if strings.Contains(got, "\ningested:") {
		t.Errorf("an already-ingested file was listed as ingested: %q", got)
	}
}

func TestIngestReportsWhatItSkipped(t *testing.T) {
	e := newEnv(t)
	e.create("w")
	dir := filepath.Join(t.TempDir(), "ctx")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "real.txt"), []byte("real"), 0o644)
	if err := os.Symlink(filepath.Join(dir, "real.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got := e.mustCall("WikiIngest", map[string]any{"slug": "w", "sources": []string{dir}})
	contains(t, got, "ingested: ctx/real.txt", "skipped: ctx/link.txt: symlink")
	e.ai.waitStarted(t, 1)
	e.waitStatus("w", wiki.StatusReady)
}

func TestIngestErrorsLeaveTheWikiUntouched(t *testing.T) {
	e := newEnv(t)
	e.create("w")
	gate := make(chan struct{})
	e.ai.setHold(maintainer.OpIngest, gate)
	e.mustCall("WikiIngest", map[string]any{"slug": "w", "files": []map[string]string{{"name": "a.md", "content": "one"}}})
	e.ai.waitStarted(t, 1)

	// Busy: refused with who holds it.
	out := e.call("WikiIngest", map[string]any{"slug": "w", "files": []map[string]string{{"name": "z.md", "content": "z"}}})
	if !out.IsError {
		t.Fatalf("a busy wiki accepted an ingest: %q", out.Text)
	}
	contains(t, out.Text, "busy", "ingest")
	close(gate)
	e.waitStatus("w", wiki.StatusReady)

	for _, c := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"nothing to ingest", map[string]any{"slug": "w"}, "nothing to ingest"},
		{"no slug", map[string]any{"files": []map[string]string{{"name": "a", "content": "x"}}}, "required"},
		{"unknown wiki", map[string]any{"slug": "nope", "files": []map[string]string{{"name": "a", "content": "x"}}}, "not found"},
		{"clash", map[string]any{"slug": "w", "files": []map[string]string{{"name": "a.md", "content": "DIFFERENT"}}}, "conflicts"},
		{"bad file name", map[string]any{"slug": "w", "files": []map[string]string{{"name": "../escape", "content": "x"}}}, "invalid"},
		{"missing source", map[string]any{"slug": "w", "sources": []string{"/definitely/not/here"}}, ""},
	} {
		out := e.call("WikiIngest", c.args)
		if !out.IsError || !strings.Contains(out.Text, c.want) {
			t.Errorf("%s: %+v, want an error mentioning %q", c.name, out, c.want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(e.root, "w", "raw", "a.md")); string(b) != "one" {
		t.Errorf("a refused ingest changed the wiki: raw/a.md = %q", b)
	}
	if _, err := os.Stat(filepath.Join(e.root, "w", "raw", "z.md")); err == nil {
		t.Error("the file of a refused ingest was written")
	}
	if info, _ := e.st.Get("w"); info.Status != wiki.StatusReady {
		t.Errorf("status %s after refused ingests, want ready", info.Status)
	}
}

func TestIngestAfterAFailedRunIsToldTheWikiWasInterrupted(t *testing.T) {
	e := newEnv(t)
	e.create("w")
	e.ai.setFail(context.Canceled)
	e.mustCall("WikiIngest", map[string]any{"slug": "w", "files": []map[string]string{{"name": "a.md", "content": "x"}}})
	e.ai.waitStarted(t, 1)
	e.waitStatus("w", wiki.StatusIncomplete)

	e.ai.setFail(nil)
	got := e.mustCall("WikiIngest", map[string]any{"slug": "w", "files": []map[string]string{{"name": "b.md", "content": "y"}}})
	contains(t, got, "cut short", "coherence")
}

func TestLint(t *testing.T) {
	e := newEnv(t)
	e.create("w")
	gate := make(chan struct{})
	e.ai.setHold(maintainer.OpLint, gate)

	got := e.mustCall("WikiLint", map[string]any{"slug": "w", "guidance": "check the links", "model": "p/m"})
	contains(t, got, "Lint started", "Status: busy (lint)", "WikiList")
	e.ai.waitStarted(t, 1)
	if run := e.ai.last(); run.Op != maintainer.OpLint || run.Model != "p/m" || !strings.Contains(run.Prompt, "check the links") {
		t.Errorf("the maintainer ran as %+v", run)
	}
	if out := e.call("WikiLint", map[string]any{"slug": "w"}); !out.IsError || !strings.Contains(out.Text, "busy") {
		t.Errorf("a second lint on a busy wiki: %+v, want a busy error", out)
	}
	close(gate)
	e.waitStatus("w", wiki.StatusReady)

	for _, c := range []struct {
		args any
		want string
	}{
		{map[string]any{}, "required"},
		{map[string]any{"slug": "nope"}, "not found"},
	} {
		if out := e.call("WikiLint", c.args); !out.IsError || !strings.Contains(out.Text, c.want) {
			t.Errorf("lint %v: %+v, want %q", c.args, out, c.want)
		}
	}
}

func TestQueryReturnsTheAnswerAndPassesItsArguments(t *testing.T) {
	e := newEnv(t)
	e.create("w")
	e.ai.mu.Lock()
	e.ai.reply = "42, see [[answer]]"
	e.ai.mu.Unlock()
	got := e.mustCall("WikiQuery", map[string]any{"slug": "w", "question": "what is the answer?", "timeout": 60, "model": "p/m"})
	if got != "42, see [[answer]]" {
		t.Errorf("answer %q, want the maintainer's text verbatim", got)
	}
	e.ai.waitStarted(t, 1)
	if run := e.ai.last(); run.Op != maintainer.OpQuery || run.Slug != "w" || run.Model != "p/m" || !strings.Contains(run.Prompt, "what is the answer?") {
		t.Errorf("the maintainer ran as %+v", run)
	}
}

func TestQueryErrors(t *testing.T) {
	e := newEnv(t)
	e.create("w")
	for _, c := range []struct {
		name string
		args any
		want string
	}{
		{"no question", map[string]any{"slug": "w"}, `invalid input: "question" is required`},
		{"blank question", map[string]any{"slug": "w", "question": "  "}, `invalid input: "question" is required`},
		{"no slug", map[string]any{"question": "q"}, `invalid input: "slug" is required`},
		{"unknown wiki", map[string]any{"slug": "nope", "question": "q"}, "not found"},
		{"timeout of the wrong type", map[string]any{"slug": "w", "question": "q", "timeout": "soon"}, "arguments"},
	} {
		if out := e.call("WikiQuery", c.args); !out.IsError || !strings.Contains(out.Text, c.want) {
			t.Errorf("%s: %+v, want an error mentioning %q", c.name, out, c.want)
		}
	}
}

func TestQueryTakesNoLockAndRunsConcurrentlyWithAnIngest(t *testing.T) {
	e := newEnv(t)
	e.create("w")
	gate := make(chan struct{})
	e.ai.setHold(maintainer.OpIngest, gate)
	e.mustCall("WikiIngest", map[string]any{"slug": "w", "files": []map[string]string{{"name": "a.md", "content": "x"}}})
	e.ai.waitStarted(t, 1)
	e.waitStatus("w", wiki.StatusBusy)

	// While the ingest holds the wiki, several queries run at once and all answer.
	qgate := make(chan struct{})
	e.ai.setHold(maintainer.OpQuery, qgate)
	const n = 5
	results := make(chan callOut, n)
	for i := 0; i < n; i++ {
		go func() {
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
				"params": map[string]any{"name": "WikiQuery", "arguments": map[string]any{"slug": "w", "question": "q"}}})
			resp, err := http.Post(e.srv.URL, "application/json", bytes.NewReader(b))
			if err != nil {
				results <- callOut{err.Error(), true}
				return
			}
			defer resp.Body.Close()
			var r struct {
				Result struct {
					Content []struct{ Text string }
					IsError bool `json:"isError"`
				}
			}
			json.NewDecoder(resp.Body).Decode(&r)
			text := ""
			if len(r.Result.Content) > 0 {
				text = r.Result.Content[0].Text
			}
			results <- callOut{text, r.Result.IsError}
		}()
	}
	e.ai.waitStarted(t, n) // all five are inside the maintainer at the same time
	close(qgate)
	for i := 0; i < n; i++ {
		if out := <-results; out.IsError {
			t.Errorf("query %d failed while the wiki was busy: %s", i, out.Text)
		}
	}
	close(gate)
	e.waitStatus("w", wiki.StatusReady)
}

func TestQueryTimeoutIsAToolErrorAndTheCapIsApplied(t *testing.T) {
	e := newEnv(t)
	e.create("w")
	e.ai.setHold(maintainer.OpQuery, make(chan struct{})) // never released
	start := time.Now()
	out := e.call("WikiQuery", map[string]any{"slug": "w", "question": "q", "timeout": 0.2})
	if !out.IsError {
		t.Fatalf("a timed-out query succeeded: %q", out.Text)
	}
	contains(t, out.Text, "timeout", "retry with a larger timeout")
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the 0.2 s timeout took %v", d)
	}
}

func TestAQueryStopsWhenTheClientGoesAway(t *testing.T) {
	// A caller that hangs up must not leave a maintainer burning tokens for an answer nobody reads.
	e := newEnv(t)
	e.create("w")
	e.ai.setHold(maintainer.OpQuery, make(chan struct{})) // never released
	ctx, cancel := context.WithCancel(context.Background())
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"WikiQuery","arguments":{"slug":"w","question":"q"}}}`
	req, _ := http.NewRequestWithContext(ctx, "POST", e.srv.URL, strings.NewReader(body))
	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	e.ai.waitStarted(t, 1)
	cancel()
	select {
	case op := <-e.ai.cancelled:
		if op != maintainer.OpQuery {
			t.Errorf("cancelled %s, want the query", op)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the maintainer kept running after the client hung up")
	}
	<-done
}

func TestToolErrorsAreResultsNotJSONRPCErrors(t *testing.T) {
	// The model must be able to read the failure and recover; a JSON-RPC error would surface as a
	// transport failure instead.
	e := newEnv(t)
	r := e.rpc("tools/call", map[string]any{"name": "WikiDelete", "arguments": map[string]any{"slug": "nope"}})
	if r.Error != nil {
		t.Fatalf("got a JSON-RPC error %+v", r.Error)
	}
	var res struct {
		IsError bool `json:"isError"`
	}
	json.Unmarshal(r.Result, &res)
	if !res.IsError {
		t.Errorf("result %s has no isError", r.Result)
	}
	// And a success does not carry the flag at all.
	ok := e.rpc("tools/call", map[string]any{"name": "WikiList"})
	if strings.Contains(string(ok.Result), "isError") {
		t.Errorf("a successful result carries isError: %s", ok.Result)
	}
}

func TestMissingArgumentsAreTreatedAsAnEmptyObject(t *testing.T) {
	e := newEnv(t)
	// A tool that parses its arguments must report the missing field, not a decode failure.
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"WikiDelete"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"WikiDelete","arguments":null}}`,
	} {
		_, out := e.post(raw)
		if !strings.Contains(out, `\"slug\" is required`) || strings.Contains(out, "unexpected end") {
			t.Errorf("%s → %s, want the missing-slug error", raw, out)
		}
	}
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"WikiList"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"WikiList","arguments":null}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"WikiList","arguments":{}}}`,
	} {
		if code, out := e.post(raw); code != 200 || strings.Contains(out, `"error"`) {
			t.Errorf("%s → %d %s", raw, code, out)
		}
	}
}

func TestTimeoutOf(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want time.Duration
	}{
		{0, 0}, {-5, 0}, {1, time.Second}, {0.5, 500 * time.Millisecond}, {90, 90 * time.Second},
		{service.MaxTimeout.Seconds(), service.MaxTimeout},
		{1e12, service.MaxTimeout}, // a huge value must clamp, not overflow into a negative duration
	} {
		if got := timeoutOf(c.in); got != c.want {
			t.Errorf("timeoutOf(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// ── the real harness client ─────────────────────────────────────────────────

// TestHarnessSDKClientWorksAgainstTheServer drives the server with the MCP client harness itself
// uses (mcp.NewHTTPTransport), so the compatibility claim — "a harness points at /mcp and gets
// the tools" — is proven against the real thing, not against this package's idea of the protocol.
func TestHarnessSDKClientWorksAgainstTheServer(t *testing.T) {
	e := newEnv(t)
	tr, err := harnessmcp.NewHTTPTransport(harnessmcp.HTTPConfig{URL: e.srv.URL, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	c := harnessmcp.NewClient(tr)
	defer c.Close()
	ctx := context.Background()

	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var names []string
	for _, tl := range tools {
		names = append(names, tl.Name)
		if tl.Description == "" || len(tl.InputSchema) == 0 {
			t.Errorf("%s arrives without a description or schema", tl.Name)
		}
	}
	if strings.Join(names, ",") != "WikiList,WikiCreate,WikiDelete,WikiIngest,WikiQuery,WikiLint" {
		t.Fatalf("tools %v", names)
	}

	res, err := c.CallTool(ctx, "WikiCreate", json.RawMessage(`{"slug":"via-sdk","description":"made through the harness client"}`))
	if err != nil || res.IsError {
		t.Fatalf("WikiCreate: %v %+v", err, res)
	}
	contains(t, res.Text(), "via-sdk", "created")

	res, err = c.CallTool(ctx, "WikiList", nil)
	if err != nil || res.IsError || !strings.Contains(res.Text(), `"via-sdk"`) {
		t.Fatalf("WikiList: %v %+v", err, res)
	}
	res, err = c.CallTool(ctx, "WikiQuery", json.RawMessage(`{"slug":"via-sdk","question":"anything?"}`))
	if err != nil || res.IsError || res.Text() != "the maintainer says: done" {
		t.Fatalf("WikiQuery: %v %+v", err, res)
	}
	// A tool failure reaches the client as a result with IsError — not as a Go error.
	res, err = c.CallTool(ctx, "WikiQuery", json.RawMessage(`{"slug":"nope","question":"q"}`))
	if err != nil || !res.IsError || !strings.Contains(res.Text(), "not found") {
		t.Fatalf("a failing tool: err=%v res=%+v, want IsError with the reason", err, res)
	}
	res, err = c.CallTool(ctx, "WikiDelete", json.RawMessage(`{"slug":"via-sdk"}`))
	if err != nil || res.IsError {
		t.Fatalf("WikiDelete: %v %+v", err, res)
	}
}
