package client

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// wikiServer records the last request and answers with a canned status and body.
type wikiServer struct {
	*httptest.Server
	method, path, body string
	status             int
	reply              string
}

func newWikiServer(t *testing.T, status int, reply string) *wikiServer {
	t.Helper()
	ws := &wikiServer{status: status, reply: reply}
	ws.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ws.method, ws.path, ws.body = r.Method, r.URL.EscapedPath(), string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ws.status)
		io.WriteString(w, ws.reply)
	}))
	t.Cleanup(ws.Close)
	return ws
}

func (ws *wikiServer) client() *Client { return New(ws.URL) }

func TestWikiListDecodesAndNeverReturnsNil(t *testing.T) {
	ws := newWikiServer(t, 200, `[{"slug":"a","description":"first","created_at":"2026-11-15T10:00:00Z","status":"busy","op":"ingest","since":"2026-11-15T10:05:00Z"},{"slug":"b","description":"second","created_at":"2026-11-15T10:00:00Z","status":"ready"}]`)
	list, err := ws.client().WikiList()
	if err != nil {
		t.Fatal(err)
	}
	if ws.method != "GET" || ws.path != "/api/wiki" {
		t.Fatalf("request = %s %s", ws.method, ws.path)
	}
	if len(list) != 2 || list[0].Slug != "a" || list[0].Status != "busy" || list[0].Op != "ingest" || list[0].Since.IsZero() || list[1].Status != "ready" || !list[1].Since.IsZero() {
		t.Fatalf("list = %+v", list)
	}
	// a null body from a custom server still gives an empty slice, not nil
	ws2 := newWikiServer(t, 200, `null`)
	if got, err := ws2.client().WikiList(); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("WikiList on null = %#v, %v; want a non-nil empty slice", got, err)
	}
}

func TestWikiCreateSendsSlugAndDescription(t *testing.T) {
	ws := newWikiServer(t, 201, `{"slug":"sgw","description":"SGW","status":"ready","created_at":"2026-11-15T10:00:00Z"}`)
	info, err := ws.client().WikiCreate("sgw", "SGW")
	if err != nil {
		t.Fatal(err)
	}
	if ws.method != "POST" || ws.path != "/api/wiki" {
		t.Fatalf("request = %s %s", ws.method, ws.path)
	}
	var sent map[string]string
	json.Unmarshal([]byte(ws.body), &sent)
	if sent["slug"] != "sgw" || sent["description"] != "SGW" {
		t.Fatalf("body = %s", ws.body)
	}
	if info.Slug != "sgw" || info.Status != "ready" {
		t.Fatalf("info = %+v", info)
	}
}

func TestWikiDeleteAndSlugEscaping(t *testing.T) {
	ws := newWikiServer(t, 200, `{"status":{"code":"deleted"}}`)
	st, err := ws.client().WikiDelete("gone")
	if err != nil || st.Code != "deleted" {
		t.Fatalf("%+v %v", st, err)
	}
	if ws.method != "DELETE" || ws.path != "/api/wiki/gone" {
		t.Fatalf("request = %s %s", ws.method, ws.path)
	}
	// whatever the caller passes stays inside the one path segment: it can never reach
	// another route
	for _, evil := range []string{"../x", "a/b", "a?b=c", "a#d", "a b"} {
		ws.client().WikiDelete(evil)
		if strings.Count(strings.TrimPrefix(ws.path, "/api/wiki/"), "/") != 0 || strings.Contains(ws.path, "?") {
			t.Errorf("slug %q produced the path %q, which is not a single segment", evil, ws.path)
		}
	}
}

func TestWikiIngestSendsSourcesFilesAndGuidance(t *testing.T) {
	ws := newWikiServer(t, 202, `{"slug":"w","status":"busy","op":"ingest","ingested":["ctx/a.md","n.md"],"already_ingested":["old.md"],"skipped":["ctx/l: symlink"]}`)
	acc, err := ws.client().WikiIngest("w", WikiSources{
		Paths:    []string{"/data/ctx"},
		Files:    []WikiFile{{Name: "n.md", Content: "N"}},
		Guidance: "focus on the CAN bus",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ws.method != "POST" || ws.path != "/api/wiki/w/ingest" {
		t.Fatalf("request = %s %s", ws.method, ws.path)
	}
	var sent struct {
		Sources  []string   `json:"sources"`
		Files    []WikiFile `json:"files"`
		Guidance string     `json:"guidance"`
	}
	json.Unmarshal([]byte(ws.body), &sent)
	if len(sent.Sources) != 1 || sent.Sources[0] != "/data/ctx" || len(sent.Files) != 1 || sent.Files[0].Name != "n.md" || sent.Guidance != "focus on the CAN bus" {
		t.Fatalf("body = %s", ws.body)
	}
	if acc.Status != "busy" || acc.Op != "ingest" || len(acc.Ingested) != 2 || acc.AlreadyIngested[0] != "old.md" || acc.Skipped[0] != "ctx/l: symlink" {
		t.Fatalf("accepted = %+v", acc)
	}
	// empty optional fields are not sent at all
	ws2 := newWikiServer(t, 202, `{"slug":"w"}`)
	ws2.client().WikiIngest("w", WikiSources{Files: []WikiFile{{Name: "a.md", Content: "A"}}})
	if strings.Contains(ws2.body, `"sources"`) || strings.Contains(ws2.body, `"guidance"`) {
		t.Errorf("empty fields were sent: %s", ws2.body)
	}
}

func TestWikiLintBodyIsOptional(t *testing.T) {
	ws := newWikiServer(t, 202, `{"slug":"w","status":"busy","op":"lint"}`)
	acc, err := ws.client().WikiLint("w", "")
	if err != nil || acc.Op != "lint" {
		t.Fatalf("%+v %v", acc, err)
	}
	if ws.method != "POST" || ws.path != "/api/wiki/w/lint" || ws.body != "" {
		t.Fatalf("a lint with no guidance: %s %s body=%q, want no body", ws.method, ws.path, ws.body)
	}
	ws.client().WikiLint("w", "only chrysler")
	if !strings.Contains(ws.body, `"guidance":"only chrysler"`) {
		t.Fatalf("body = %q", ws.body)
	}
}

func TestWikiQueryReturnsTheAnswerAndSendsTheTimeout(t *testing.T) {
	ws := newWikiServer(t, 200, `{"slug":"w","answer":"the answer, see wiki/x.md"}`)
	got, err := ws.client().WikiQuery("w", "how?", 90*time.Second)
	if err != nil || got != "the answer, see wiki/x.md" {
		t.Fatalf("%q %v", got, err)
	}
	if ws.method != "POST" || ws.path != "/api/wiki/w/query" {
		t.Fatalf("request = %s %s", ws.method, ws.path)
	}
	var sent map[string]any
	json.Unmarshal([]byte(ws.body), &sent)
	if sent["question"] != "how?" || sent["timeout"] != float64(90) {
		t.Fatalf("body = %s", ws.body)
	}
	// a timeout is rounded UP to whole seconds, never down to 0 (which would mean "default")
	ws.client().WikiQuery("w", "q", 1500*time.Millisecond)
	json.Unmarshal([]byte(ws.body), &sent)
	if sent["timeout"] != float64(2) {
		t.Errorf("1.5 s sent as %v, want 2", sent["timeout"])
	}
	ws.client().WikiQuery("w", "q", 0)
	if strings.Contains(ws.body, `"timeout"`) {
		t.Errorf("no timeout was asked for, but one was sent: %s", ws.body)
	}
}

// The call gives up on its own — after the server-side timeout plus some slack — when the
// server never answers, instead of hanging forever.
func TestWikiQueryGivesUpWhenTheServerNeverAnswers(t *testing.T) {
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(hold); srv.Close() })
	t0 := time.Now()
	_, err := New(srv.URL).WikiQuery("w", "q", 200*time.Millisecond)
	if err == nil {
		t.Fatal("a query nobody answered returned no error")
	}
	if el := time.Since(t0); el < 5*time.Second || el > 20*time.Second {
		t.Errorf("gave up after %v, want about the timeout + 10 s of slack", el)
	}
}

func TestWikiErrorsAreClassifiedByStatusAndDetails(t *testing.T) {
	busy := newWikiServer(t, 409, `{"error":{"message":"wiki \"w\" is busy (ingest, 4m)","details":{"slug":"w","op":"ingest","since":"2026-11-15T10:00:00Z","elapsed_seconds":240}}}`)
	_, err := busy.client().WikiLint("w", "")
	if !IsWikiBusy(err) {
		t.Fatalf("IsWikiBusy(%v) = false", err)
	}
	if _, ok := IsWikiConflict(err); ok {
		t.Error("a busy wiki must not be reported as a raw/ conflict")
	}
	var ae *Error
	if !asError(err, &ae) || ae.Details["op"] != "ingest" || ae.Details["elapsed_seconds"] != float64(240) {
		t.Errorf("the busy details were lost: %+v", err)
	}

	clash := newWikiServer(t, 409, `{"error":{"message":"conflicts in raw/","details":{"conflicts":["a.md","ctx/b.md"]}}}`)
	_, err = clash.client().WikiIngest("w", WikiSources{Files: []WikiFile{{Name: "a.md", Content: "x"}}})
	paths, ok := IsWikiConflict(err)
	if !ok || len(paths) != 2 || paths[0] != "a.md" || paths[1] != "ctx/b.md" {
		t.Fatalf("IsWikiConflict = %v, %v", paths, ok)
	}
	if IsWikiBusy(err) {
		t.Error("a raw/ conflict must not be reported as busy")
	}

	// a 409 that is neither (a wiki that already exists) is neither
	exists := newWikiServer(t, 409, `{"error":{"message":"wiki already exists: \"w\""}}`)
	_, err = exists.client().WikiCreate("w", "d")
	if IsWikiBusy(err) {
		t.Error("'already exists' is not 'busy'")
	}
	if _, ok := IsWikiConflict(err); ok {
		t.Error("'already exists' is not a raw/ conflict")
	}

	off := newWikiServer(t, 503, `{"error":{"message":"wikis are not enabled on this agent"}}`)
	_, err = off.client().WikiList()
	if !IsWikiUnavailable(err) || IsWikiBusy(err) {
		t.Errorf("a 503: unavailable=%v busy=%v", IsWikiUnavailable(err), IsWikiBusy(err))
	}
	// "busy" is a 409 WITH the operation's details: the same details on another status are
	// not busy (they could only come from something that is not the lock refusing)
	for _, status := range []int{400, 404, 500, 503} {
		odd := &Error{Status: status, Message: "x", Details: map[string]any{"slug": "w", "op": "ingest"}}
		if IsWikiBusy(odd) {
			t.Errorf("a %d carrying busy-looking details was classified as busy", status)
		}
	}
	// and a 409 without the details is not busy either
	if IsWikiBusy(&Error{Status: 409, Message: "x"}) || IsWikiBusy(&Error{Status: 409, Message: "x", Details: map[string]any{"op": "ingest"}}) {
		t.Error("a 409 without slug and op was classified as busy")
	}
	// nil and unrelated errors are none of these
	for _, e := range []error{nil, io.EOF, &Error{Status: 404, Message: "x"}} {
		if IsWikiBusy(e) || IsWikiUnavailable(e) {
			t.Errorf("%v classified as busy/unavailable", e)
		}
		if _, ok := IsWikiConflict(e); ok {
			t.Errorf("%v classified as a conflict", e)
		}
	}
}

func (ws *wikiServer) errOf(t *testing.T) error {
	_, err := ws.client().WikiLint("w", "")
	return err
}

func asError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
