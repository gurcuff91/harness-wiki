package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gurcuff91/harness-wiki/internal/version"
	"github.com/gurcuff91/harness-wiki/maintainer"
	"github.com/gurcuff91/harness-wiki/service"
	"github.com/gurcuff91/harness-wiki/wiki"
)

// fakeAI answers a run, or holds it. gate decides per run (by operation) whether to wait.
type fakeAI struct {
	mu    sync.Mutex
	runs  []maintainer.Run
	hold  map[maintainer.Op]chan struct{}
	reply string
	fail  error
	seen  chan maintainer.Run
}

func newFakeAI() *fakeAI {
	return &fakeAI{reply: "the maintainer says: done", hold: map[maintainer.Op]chan struct{}{}, seen: make(chan maintainer.Run, 128)}
}

func (f *fakeAI) Execute(ctx context.Context, run maintainer.Run) (string, error) {
	f.mu.Lock()
	f.runs = append(f.runs, run)
	gate, reply, fail := f.hold[run.Op], f.reply, f.fail
	f.mu.Unlock()
	f.seen <- run
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "partial", ctx.Err()
		}
	}
	return reply, fail
}

func (f *fakeAI) setHold(op maintainer.Op, ch chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch == nil {
		delete(f.hold, op)
		return
	}
	f.hold[op] = ch
}

func (f *fakeAI) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.runs) }

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

func (e *env) do(method, path, body string) (int, map[string]any, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	json.Unmarshal(raw, &m)
	return resp.StatusCode, m, string(raw)
}

func errMsg(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	s, _ := e["message"].(string)
	return s
}

func errDetails(m map[string]any) map[string]any {
	e, _ := m["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	return d
}

func (e *env) create(slug string) {
	e.t.Helper()
	b, _ := json.Marshal(map[string]string{"slug": slug, "description": "d"})
	if code, _, raw := e.do("POST", "/api/wiki", string(b)); code != http.StatusCreated {
		e.t.Fatalf("create %s: %d %s", slug, code, raw)
	}
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

func (e *env) waitRuns(n int) {
	e.t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-e.ai.seen:
		case <-time.After(5 * time.Second):
			e.t.Fatalf("only %d of %d runs started", i, n)
		}
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// ── list and create ─────────────────────────────────────────────────────────

func TestListIsAnEmptyArrayNotNullAndCreatesNothingOnDisk(t *testing.T) {
	e := newEnv(t)
	code, _, raw := e.do("GET", "/api/wiki", "")
	if code != 200 || strings.TrimSpace(raw) != "[]" {
		t.Fatalf("empty list: %d %q, want 200 and []", code, raw)
	}
	if exists(e.root) {
		t.Fatal("listing created the store root: nothing may touch the disk before the first create")
	}
}

func TestCreateThenList(t *testing.T) {
	e := newEnv(t)
	code, m, raw := e.do("POST", "/api/wiki", `{"slug":"chrysler-sgw","description":"SGW research","model":"p/m"}`)
	if code != http.StatusCreated || m["slug"] != "chrysler-sgw" || m["description"] != "SGW research" || m["status"] != "ready" {
		t.Fatalf("create: %d %s", code, raw)
	}
	for _, f := range []string{"wiki.json", "AGENTS.md", "raw", "wiki/index.md", "wiki/log.md"} {
		if !exists(filepath.Join(e.root, "chrysler-sgw", filepath.FromSlash(f))) {
			t.Errorf("create did not make %s", f)
		}
	}
	if r := <-e.ai.seen; r.Op != maintainer.OpInit || r.Model != "p/m" {
		t.Errorf("the schema adaptation ran as %+v, want init on model p/m", r)
	}
	code, _, raw = e.do("GET", "/api/wiki", "")
	var list []map[string]any
	if json.Unmarshal([]byte(raw), &list); code != 200 || len(list) != 1 || list[0]["slug"] != "chrysler-sgw" || list[0]["status"] != "ready" {
		t.Fatalf("list: %d %s", code, raw)
	}
}

func TestCreateErrors(t *testing.T) {
	e := newEnv(t)
	e.create("taken")
	for _, c := range []struct {
		name, body string
		want       int
		msg        string
	}{
		{"invalid slug", `{"slug":"Bad Slug","description":"d"}`, 400, "invalid wiki slug"},
		{"path-like slug", `{"slug":"../x","description":"d"}`, 400, "invalid wiki slug"},
		{"reserved slug", `{"slug":"con","description":"d"}`, 400, "reserved"},
		{"no slug", `{"description":"d"}`, 400, "slug"},
		{"no description", `{"slug":"ok"}`, 400, "description"},
		{"blank description", `{"slug":"ok","description":"  "}`, 400, "description"},
		{"already exists", `{"slug":"taken","description":"d"}`, 409, "already exists"},
		{"not json", `{nope`, 400, "invalid body"},
	} {
		code, m, raw := e.do("POST", "/api/wiki", c.body)
		if code != c.want || !strings.Contains(errMsg(m), c.msg) {
			t.Errorf("%s: %d %s, want %d containing %q", c.name, code, raw, c.want, c.msg)
		}
	}
	if list, _ := e.st.List(); len(list) != 1 {
		t.Errorf("a refused create left wikis behind: %+v", list)
	}
}

func TestCreateSurvivesAFailedAdaptation(t *testing.T) {
	e := newEnv(t)
	e.ai.fail = fmt.Errorf("model refused the request")
	code, m, raw := e.do("POST", "/api/wiki", `{"slug":"w","description":"d"}`)
	if code != http.StatusCreated || m["slug"] != "w" {
		t.Fatalf("create with a failing model: %d %s — the wiki was created, so 201", code, raw)
	}
	if _, err := e.st.Get("w"); err != nil {
		t.Fatalf("the wiki does not exist: %v", err)
	}
}

// ── delete ──────────────────────────────────────────────────────────────────

func TestDelete(t *testing.T) {
	e := newEnv(t)
	e.create("gone")
	if code, m, raw := e.do("DELETE", "/api/wiki/gone", ""); code != 200 || fmt.Sprint(m["status"]) == "" {
		t.Fatalf("delete: %d %s", code, raw)
	}
	if list, _ := e.st.List(); len(list) != 0 {
		t.Fatalf("the wiki survived: %+v", list)
	}
	if code, m, _ := e.do("DELETE", "/api/wiki/gone", ""); code != 404 || !strings.Contains(errMsg(m), "not found") {
		t.Fatalf("deleting again: %d %v, want 404", code, m)
	}
	if code, _, _ := e.do("DELETE", "/api/wiki/Bad..Slug", ""); code != 400 {
		t.Fatalf("a bad slug: %d, want 400", code)
	}
}

// ── ingest ──────────────────────────────────────────────────────────────────

func TestIngestAnswers202WithTheFilesAndWorksInTheBackground(t *testing.T) {
	e := newEnv(t)
	e.create("w")
	<-e.ai.seen
	gate := make(chan struct{})
	e.ai.setHold(maintainer.OpIngest, gate)

	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "ctx", "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "ctx", "a.md"), []byte("A"), 0o644)
	os.WriteFile(filepath.Join(src, "ctx", "sub", "b.md"), []byte("B"), 0o644)
	body, _ := json.Marshal(map[string]any{
		"sources": []string{filepath.Join(src, "ctx")}, "files": []map[string]string{{"name": "notes/n.md", "content": "N"}},
		"guidance": "focus on the CAN bus", "model": "p/m",
	})
	t0 := time.Now()
	code, m, raw := e.do("POST", "/api/wiki/w/ingest", string(body))
	if code != http.StatusAccepted {
		t.Fatalf("ingest: %d %s, want 202", code, raw)
	}
	if time.Since(t0) > 3*time.Second {
		t.Errorf("ingest took %v: it must answer as soon as the sources are copied", time.Since(t0))
	}
	var got []string
	for _, f := range m["ingested"].([]any) {
		got = append(got, fmt.Sprint(f))
	}
	if strings.Join(got, ",") != "ctx/a.md,ctx/sub/b.md,notes/n.md" {
		t.Fatalf("ingested = %v, want paths relative to raw/", got)
	}
	if m["status"] != "busy" || m["op"] != "ingest" {
		t.Errorf("the 202 must show the wiki as busy/ingest: %s", raw)
	}
	for _, leak := range []string{"harness-wiki-", "/tmp", "/var/folders", "result file"} {
		if strings.Contains(raw, leak) {
			t.Errorf("the 202 leaks a server-side detail %q: %s", leak, raw)
		}
	}
	for rel, want := range map[string]string{"ctx/a.md": "A", "ctx/sub/b.md": "B", "notes/n.md": "N"} {
		if b, err := os.ReadFile(filepath.Join(e.root, "w", "raw", filepath.FromSlash(rel))); err != nil || string(b) != want {
			t.Errorf("raw/%s = %q, %v", rel, b, err)
		}
	}
	r := <-e.ai.seen
	if r.Op != maintainer.OpIngest || r.Model != "p/m" || !strings.Contains(r.Prompt, "focus on the CAN bus") {
		t.Errorf("the maintainer ran as %+v", r)
	}
	// while it works: the list says busy, and the other long operations are refused with details
	if _, _, listRaw := e.do("GET", "/api/wiki", ""); !strings.Contains(listRaw, `"busy"`) {
		t.Errorf("GET /api/wiki while ingesting = %s, want busy", listRaw)
	}
	time.Sleep(1200 * time.Millisecond) // a known amount of busy time, to check elapsed_seconds
	code, m, raw = e.do("POST", "/api/wiki/w/lint", `{}`)
	if code != http.StatusConflict {
		t.Fatalf("lint while busy: %d %s, want 409", code, raw)
	}
	d := errDetails(m)
	if d["op"] != "ingest" || d["slug"] != "w" || d["since"] == nil {
		t.Errorf("409 details = %v, want slug, op and since", d)
	}
	if el, ok := d["elapsed_seconds"].(float64); !ok || el < 1 || el > 30 {
		t.Errorf("elapsed_seconds = %v, want about 1-2", d["elapsed_seconds"])
	}
	if _, err := time.Parse(time.RFC3339, fmt.Sprint(d["since"])); err != nil {
		t.Errorf("since = %v, want RFC 3339", d["since"])
	}
	if code, _, _ := e.do("DELETE", "/api/wiki/w", ""); code != http.StatusConflict {
		t.Errorf("a delete while busy: %d, want 409", code)
	}
	close(gate)
	e.waitStatus("w", wiki.StatusReady)
	if exists(filepath.Join(e.root, "w", ".pending")) {
		t.Error("a finished ingest left its pending marker")
	}
}

func TestIngestErrors(t *testing.T) {
	e := newEnv(t)
	e.create("w")
	e.do("POST", "/api/wiki/w/ingest", `{"files":[{"name":"keep.md","content":"ORIGINAL"}]}`)
	e.waitStatus("w", wiki.StatusReady)
	runs := e.ai.count()

	for _, c := range []struct {
		name, path, body string
		want             int
		msg              string
	}{
		{"nothing to ingest", "/api/wiki/w/ingest", `{}`, 400, "nothing to ingest"},
		{"a missing source", "/api/wiki/w/ingest", `{"sources":["/definitely/not/here.md"]}`, 400, "source"},
		{"a glob that matches nothing", "/api/wiki/w/ingest", fmt.Sprintf(`{"sources":[%q]}`, filepath.Join(t.TempDir(), "*.nope")), 400, "matches no files"},
		{"an escaping name", "/api/wiki/w/ingest", `{"files":[{"name":"../escape.md","content":"x"}]}`, 400, "invalid path"},
		{"an absolute name", "/api/wiki/w/ingest", `{"files":[{"name":"/etc/x","content":"x"}]}`, 400, "invalid path"},
		{"unknown wiki", "/api/wiki/nope/ingest", `{"files":[{"name":"a.md","content":"x"}]}`, 404, "not found"},
		{"bad slug", "/api/wiki/Bad..Slug/ingest", `{"files":[{"name":"a.md","content":"x"}]}`, 400, "invalid wiki slug"},
		{"not json", "/api/wiki/w/ingest", `{nope`, 400, "invalid body"},
	} {
		code, m, raw := e.do("POST", c.path, c.body)
		if code != c.want || !strings.Contains(strings.ToLower(errMsg(m)), strings.ToLower(c.msg)) {
			t.Errorf("%s: %d %s, want %d containing %q", c.name, code, raw, c.want, c.msg)
		}
	}
	code, m, raw := e.do("POST", "/api/wiki/w/ingest", `{"files":[{"name":"keep.md","content":"CHANGED"},{"name":"fresh.md","content":"F"}]}`)
	if code != http.StatusConflict {
		t.Fatalf("a clash: %d %s, want 409", code, raw)
	}
	if c, _ := errDetails(m)["conflicts"].([]any); len(c) != 1 || c[0] != "keep.md" {
		t.Errorf("409 details = %v, want the clashing paths", errDetails(m))
	}
	if exists(filepath.Join(e.root, "w", "raw", "fresh.md")) {
		t.Error("a refused ingest copied a file anyway (it is all or nothing)")
	}
	if info, _ := e.st.Get("w"); info.Status != wiki.StatusReady {
		t.Errorf("a refused ingest left the wiki %s, want ready", info.Status)
	}
	if e.ai.count() != runs {
		t.Error("the maintainer ran for a refused ingest")
	}
	code, m, raw = e.do("POST", "/api/wiki/w/ingest", `{"files":[{"name":"keep.md","content":"ORIGINAL"},{"name":"new.md","content":"N"}]}`)
	if code != http.StatusAccepted {
		t.Fatalf("re-ingest: %d %s", code, raw)
	}
	if ai, _ := m["already_ingested"].([]any); len(ai) != 1 || ai[0] != "keep.md" {
		t.Errorf("already_ingested = %v in %s", m["already_ingested"], raw)
	}
}

// ── lint ────────────────────────────────────────────────────────────────────

func TestLint(t *testing.T) {
	e := newEnv(t)
	e.create("w")
	<-e.ai.seen
	gate := make(chan struct{})
	e.ai.setHold(maintainer.OpLint, gate)

	code, m, raw := e.do("POST", "/api/wiki/w/lint", "") // no body at all: guidance is optional
	if code != http.StatusAccepted || m["slug"] != "w" || m["status"] != "busy" || m["op"] != "lint" {
		t.Fatalf("lint: %d %s, want 202 busy/lint", code, raw)
	}
	e.waitRuns(1)
	if code, _, _ := e.do("POST", "/api/wiki/w/lint", `{}`); code != http.StatusConflict {
		t.Errorf("a second lint while busy: %d, want 409", code)
	}
	close(gate)
	e.waitStatus("w", wiki.StatusReady)
	if code, _, _ := e.do("POST", "/api/wiki/nope/lint", `{}`); code != 404 {
		t.Errorf("lint of an unknown wiki: %d, want 404", code)
	}
}

// ── query ───────────────────────────────────────────────────────────────────

func TestQueryAnswersSynchronously(t *testing.T) {
	e := newEnv(t)
	e.create("w")
	code, m, raw := e.do("POST", "/api/wiki/w/query", `{"question":"what do we know?"}`)
	if code != 200 || m["slug"] != "w" || m["answer"] != "the maintainer says: done" {
		t.Fatalf("query: %d %s", code, raw)
	}
	if info, _ := e.st.Get("w"); info.Status != wiki.StatusReady {
		t.Errorf("after a query the wiki is %s, want ready", info.Status)
	}
	if exists(filepath.Join(e.root, "w", ".pending")) {
		t.Error("a query left a pending marker")
	}
	for _, c := range []struct {
		body string
		want int
	}{{`{}`, 400}, {`{"question":"  "}`, 400}, {`{nope`, 400}} {
		if code, _, raw := e.do("POST", "/api/wiki/w/query", c.body); code != c.want {
			t.Errorf("query %s: %d %s, want %d", c.body, code, raw, c.want)
		}
	}
	if code, _, _ := e.do("POST", "/api/wiki/nope/query", `{"question":"q"}`); code != 404 {
		t.Errorf("unknown wiki: %d, want 404", code)
	}
}

// A query takes no lock: it is answered while an ingest or lint holds the wiki, and many run at once.
func TestQueryIsAnsweredWhileTheWikiIsBusy(t *testing.T) {
	e := newEnv(t)
	e.create("w")
	<-e.ai.seen
	gate := make(chan struct{})
	e.ai.setHold(maintainer.OpLint, gate)
	if code, _, raw := e.do("POST", "/api/wiki/w/lint", `{}`); code != http.StatusAccepted {
		t.Fatalf("lint: %d %s", code, raw)
	}
	e.waitRuns(1)
	t0 := time.Now()
	code, m, raw := e.do("POST", "/api/wiki/w/query", `{"question":"q"}`)
	if code != 200 || m["answer"] != "the maintainer says: done" {
		t.Fatalf("a query while the wiki is busy: %d %s, want 200 (a query takes no lock)", code, raw)
	}
	if time.Since(t0) > 3*time.Second {
		t.Errorf("took %v: a query must not wait for the lint", time.Since(t0))
	}
	if info, _ := e.st.Get("w"); info.Status != wiki.StatusBusy || info.Op != "lint" {
		t.Errorf("after the query the wiki is %s/%s, want still busy/lint", info.Status, info.Op)
	}
	close(gate)
	e.waitStatus("w", wiki.StatusReady)
}

func TestManyQueriesAtOnce(t *testing.T) {
	const n = 12
	e := newEnv(t)
	e.create("w")
	<-e.ai.seen
	gate := make(chan struct{})
	e.ai.setHold(maintainer.OpQuery, gate)
	var wg sync.WaitGroup
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, _, _ := e.do("POST", "/api/wiki/w/query", `{"question":"q"}`)
			codes <- code
		}()
	}
	e.waitRuns(n) // all n reached the maintainer at the same time
	close(gate)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 200 {
			t.Errorf("a concurrent query answered %d, want 200", c)
		}
	}
	if info, _ := e.st.Get("w"); info.Status != wiki.StatusReady {
		t.Errorf("after the queries the wiki is %s, want ready", info.Status)
	}
}

func TestQueryTimeoutIs504AndTheCapIsEnforced(t *testing.T) {
	e := newEnv(t)
	e.create("w")
	<-e.ai.seen
	e.ai.setHold(maintainer.OpQuery, make(chan struct{}))
	t0 := time.Now()
	code, m, raw := e.do("POST", "/api/wiki/w/query", `{"question":"slow","timeout":1}`)
	if code != http.StatusGatewayTimeout || !strings.Contains(strings.ToLower(errMsg(m)), "timeout") {
		t.Fatalf("a timed-out query: %d %s, want 504", code, raw)
	}
	if time.Since(t0) > 5*time.Second {
		t.Errorf("took %v for a 1 s timeout", time.Since(t0))
	}
	e.waitStatus("w", wiki.StatusReady)

	// the server-side cap wins over whatever the client asks for
	old := QueryMaxTimeout
	QueryMaxTimeout = time.Second
	t.Cleanup(func() { QueryMaxTimeout = old })
	t0 = time.Now()
	code, _, raw = e.do("POST", "/api/wiki/w/query", `{"question":"slow","timeout":3600}`)
	if code != http.StatusGatewayTimeout || time.Since(t0) > 5*time.Second {
		t.Fatalf("a capped query: %d after %v (%s), want 504 after about 1 s", code, time.Since(t0), raw)
	}
}

// ── routes, limits, docs ────────────────────────────────────────────────────

func TestRoutesAreExactlyTheDocumentedOnes(t *testing.T) {
	e := newEnv(t)
	e.create("w")
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/api/wiki/w", 405},
		{"PUT", "/api/wiki/w", 405},
		{"GET", "/api/wiki/w/ingest", 405},
		{"GET", "/api/wiki/w/query", 405},
		{"DELETE", "/api/wiki", 405},
		{"GET", "/api/nothing", 404},
		{"GET", "/mcp", 405},
		{"DELETE", "/mcp", 405},
		{"GET", "/mcp/", 404},
		{"GET", "/", 404},
	} {
		if code, _, _ := e.do(c.method, c.path, ""); code != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, code, c.want)
		}
	}
	if code, _, _ := e.do("GET", "/api/health", ""); code != 404 {
		t.Errorf("/api/health = %d: harness has no such endpoint, GET /api/server is the probe", code)
	}
}

func TestAnOversizedBodyIs413AndWritesNothing(t *testing.T) {
	e := newEnv(t)
	e.create("w")
	big := `{"files":[{"name":"big.md","content":"` + strings.Repeat("x", RequestLimit+1024) + `"}]}`
	if code, _, raw := e.do("POST", "/api/wiki/w/ingest", big); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized body: %d %.120s, want 413", code, raw)
	}
	if exists(filepath.Join(e.root, "w", "raw", "big.md")) {
		t.Error("an oversized request wrote a file")
	}
}

func TestOpenAPIIsServedAndMatchesTheRoutes(t *testing.T) {
	e := newEnv(t)
	code, _, raw := e.do("GET", "/api/docs/openapi.json", "")
	if code != 200 {
		t.Fatalf("openapi: %d", code)
	}
	var spec struct {
		Info  struct{ Version string }
		Paths map[string]map[string]struct {
			Responses map[string]json.RawMessage `json:"responses"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatalf("the OpenAPI document is not valid JSON: %v", err)
	}
	if spec.Info.Version == "" || strings.Contains(raw, "__VERSION__") {
		t.Errorf("the version placeholder was not filled: %q", spec.Info.Version)
	}
	want := map[string]map[string][]string{
		"/api/wiki":               {"get": {"200"}, "post": {"201", "400", "409"}},
		"/api/wiki/{slug}":        {"delete": {"200", "400", "404", "409"}},
		"/api/wiki/{slug}/ingest": {"post": {"202", "400", "404", "409", "413"}},
		"/api/wiki/{slug}/lint":   {"post": {"202", "400", "404", "409"}},
		"/api/wiki/{slug}/query":  {"post": {"200", "400", "404", "504"}}, // no 409: a query takes no lock
		"/api/server":             {"get": {"200"}},
		"/mcp":                    {"post": {"200", "202", "400", "405", "413"}},
	}
	// the document describes the server it is served from, and is titled for this project
	var head struct {
		Info    struct{ Title string }
		Servers []struct{ URL string }
	}
	json.Unmarshal([]byte(raw), &head)
	if head.Info.Title != "harness-wiki API" {
		t.Errorf("title = %q, want harness-wiki API", head.Info.Title)
	}
	if len(head.Servers) != 1 || head.Servers[0].URL != "http://"+strings.TrimPrefix(e.srv.URL, "http://") {
		t.Errorf("servers = %+v, want the address the request arrived on (%s)", head.Servers, e.srv.URL)
	}
	for path, methods := range want {
		for method, codes := range methods {
			op, ok := spec.Paths[path][method]
			if !ok {
				t.Errorf("%s %s is not documented", strings.ToUpper(method), path)
				continue
			}
			if len(op.Responses) != len(codes) {
				t.Errorf("%s %s documents %d responses, want exactly %v", strings.ToUpper(method), path, len(op.Responses), codes)
			}
			for _, c := range codes {
				if _, ok := op.Responses[c]; !ok {
					t.Errorf("%s %s does not document %s", strings.ToUpper(method), path, c)
				}
			}
		}
	}
	for path := range spec.Paths {
		if _, ok := want[path]; !ok {
			t.Errorf("%s is documented but is not a route", path)
		}
	}
	for _, s := range []string{"WikiInfo", "CreateWikiRequest", "IngestWikiRequest", "WikiFile", "LintWikiRequest", "QueryWikiRequest", "WikiAnswer", "WikiAccepted", "ErrorResponse", "Status"} {
		if _, ok := spec.Components.Schemas[s]; !ok {
			t.Errorf("schema %s is not documented", s)
		}
	}
	for _, s := range []string{"CreateWikiRequest", "IngestWikiRequest", "LintWikiRequest", "QueryWikiRequest"} {
		if !strings.Contains(string(spec.Components.Schemas[s]), `"model"`) {
			t.Errorf("%s does not document the optional per-request model", s)
		}
	}
	doc := OpenAPI("v9.9.9", "example.test:1234")
	if !strings.Contains(doc, `"version": "v9.9.9"`) || !strings.Contains(doc, `"url": "http://example.test:1234"`) {
		t.Error("OpenAPI(version, addr) did not fill the version and the server address")
	}
	if strings.Contains(doc, "__VERSION__") || strings.Contains(doc, "__ADDR__") {
		t.Error("a placeholder was left in the document")
	}
}

// The API reference page is the same Scalar UI harness serves: HTML that loads the document from
// this server (same origin, so nothing to configure).
func TestTheDocsPageIsTheScalarReferenceOverThisServersDocument(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Get(e.srv.URL + "/api/docs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("GET /api/docs = %d %q, want 200 text/html", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{"harness-wiki API Reference", `data-url="/api/docs/openapi.json"`, "@scalar/api-reference"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the docs page lacks %q", want)
		}
	}
	// the document it points at is served, as JSON
	resp2, err := http.Get(e.srv.URL + "/api/docs/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 || !strings.HasPrefix(resp2.Header.Get("Content-Type"), "application/json") {
		t.Errorf("GET /api/docs/openapi.json = %d %q, want 200 application/json", resp2.StatusCode, resp2.Header.Get("Content-Type"))
	}
	// only GET
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/docs", nil)
	if r, err := http.DefaultClient.Do(req); err == nil {
		r.Body.Close()
		if r.StatusCode != 405 {
			t.Errorf("POST /api/docs = %d, want 405", r.StatusCode)
		}
	}
}

// GET /api/server has harness's shape: name, version, url, cwd, pid, started_at. It is the
// liveness probe, so it must answer without touching a wiki or a model.
func TestServerInfoHasHarnessesShapeAndTouchesNothing(t *testing.T) {
	e := newEnv(t)
	code, m, raw := e.do("GET", "/api/server", "")
	if code != 200 {
		t.Fatalf("GET /api/server = %d %s", code, raw)
	}
	for _, f := range []string{"name", "version", "url", "cwd", "pid", "started_at"} {
		if _, ok := m[f]; !ok {
			t.Errorf("server info lacks %q: %s", f, raw)
		}
	}
	// the version is the binary's own (the one the Makefile injects) — the same value the OpenAPI
	// document carries, so a client can tell which server it is talking to
	if m["version"] != version.Version {
		t.Errorf("version = %v, want the binary's %q", m["version"], version.Version)
	}
	if m["name"] != "harness-wiki" || m["url"] != e.srv.URL {
		t.Errorf("name/url = %v / %v, want harness-wiki and %s", m["name"], m["url"], e.srv.URL)
	}
	if pid, _ := m["pid"].(float64); int(pid) != os.Getpid() {
		t.Errorf("pid = %v, want this process's %d", m["pid"], os.Getpid())
	}
	if cwd, _ := os.Getwd(); m["cwd"] != cwd {
		t.Errorf("cwd = %v, want %s", m["cwd"], cwd)
	}
	if _, err := time.Parse(time.RFC3339, fmt.Sprint(m["started_at"])); err != nil {
		t.Errorf("started_at = %v, want RFC 3339", m["started_at"])
	}
	if e.ai.count() != 0 || exists(e.root) {
		t.Error("GET /api/server ran the maintainer or created the store root")
	}
}

// /mcp is mounted on the same server and the same service as the REST API: a wiki made through one
// is seen through the other.
func TestMCPIsMountedOverTheSameServiceAsTheRESTAPI(t *testing.T) {
	e := newEnv(t)
	e.create("rest-made")
	resp, err := http.Post(e.srv.URL+"/mcp", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"WikiList"}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `rest-made`) {
		t.Fatalf("POST /mcp WikiList = %d %s, want 200 and the wiki the REST API created", resp.StatusCode, raw)
	}
	// and the other way round
	resp2, err := http.Post(e.srv.URL+"/mcp", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"WikiCreate","arguments":{"slug":"mcp-made","description":"d"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if code, _, raw := e.do("GET", "/api/wiki", ""); code != 200 || !strings.Contains(raw, "mcp-made") {
		t.Errorf("GET /api/wiki = %d %s, want the wiki the MCP tool created", code, raw)
	}
}

// /mcp must run on THE service the process built, not a copy: its background jobs have to be
// counted by the service's own Close, or a shutdown would not wait for them.
func TestMCPBackgroundJobsAreCountedByTheServicesClose(t *testing.T) {
	e := newEnv(t)
	e.create("w")
	e.svc.Close(time.Second)
	resp, err := http.Post(e.srv.URL+"/mcp", "application/json", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"WikiLint","arguments":{"slug":"w"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "shutting down") {
		t.Fatalf("a lint through /mcp after Close: %s, want it refused as the server is shutting down", raw)
	}
}
