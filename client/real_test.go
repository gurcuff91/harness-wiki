package client_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gurcuff91/harness-wiki/api"
	"github.com/gurcuff91/harness-wiki/client"
	"github.com/gurcuff91/harness-wiki/maintainer"
	"github.com/gurcuff91/harness-wiki/service"
	"github.com/gurcuff91/harness-wiki/wiki"
)

// holdAI answers every run, holding the ones whose operation has a gate.
type holdAI struct {
	mu   sync.Mutex
	gate map[maintainer.Op]chan struct{}
}

func (h *holdAI) Execute(ctx context.Context, run maintainer.Run) (string, error) {
	h.mu.Lock()
	g := h.gate[run.Op]
	h.mu.Unlock()
	if g != nil {
		select {
		case <-g:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "answer to: " + run.Op.String(), nil
}

// The typed client against the REAL handler, service and store: the contract between the two ends,
// not each end against a canned answer.
func TestTheClientAgainstTheRealServer(t *testing.T) {
	st, err := wiki.OpenFS(filepath.Join(t.TempDir(), "harness-wiki"))
	if err != nil {
		t.Fatal(err)
	}
	ai := &holdAI{gate: map[maintainer.Op]chan struct{}{}}
	svc := service.New(st, ai)
	srv := httptest.NewServer(api.New(svc))
	t.Cleanup(func() { srv.Close(); svc.Close(2 * time.Second); st.Close() })
	c := client.New(srv.URL)

	if list, err := c.WikiList(); err != nil || list == nil || len(list) != 0 {
		t.Fatalf("WikiList on a fresh server = %#v, %v", list, err)
	}
	info, err := c.WikiCreate("sgw", "Chrysler SGW research")
	if err != nil || info.Slug != "sgw" || info.Status != "ready" {
		t.Fatalf("WikiCreate = %+v, %v", info, err)
	}
	if _, err := c.WikiCreate("sgw", "again"); err == nil || client.IsWikiBusy(err) {
		t.Fatalf("creating twice: %v, want a (non-busy) conflict", err)
	}
	if _, err := c.WikiCreate("Bad Slug", "d"); err == nil {
		t.Fatal("an invalid slug must fail")
	}

	gate := make(chan struct{})
	ai.mu.Lock()
	ai.gate[maintainer.OpIngest] = gate
	ai.mu.Unlock()
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a.md"), []byte("A"), 0o644)
	acc, err := c.WikiIngest("sgw", client.WikiSources{
		Paths: []string{filepath.Join(src, "a.md")},
		Files: []client.WikiFile{{Name: "ctx/n.md", Content: "N"}},
	})
	if err != nil {
		t.Fatalf("WikiIngest: %v", err)
	}
	if acc.Slug != "sgw" || acc.Status != "busy" || acc.Op != "ingest" || len(acc.Ingested) != 2 || acc.Ingested[0] != "a.md" || acc.Ingested[1] != "ctx/n.md" {
		t.Fatalf("accepted = %+v", acc)
	}

	list, _ := c.WikiList()
	if len(list) != 1 || list[0].Status != "busy" || list[0].Op != "ingest" || list[0].Since.IsZero() {
		t.Fatalf("WikiList during the ingest = %+v", list)
	}
	// a lint is refused while the ingest holds the wiki, with what is running and since when
	_, err = c.WikiLint("sgw", "")
	var ce *client.Error
	if !client.IsWikiBusy(err) || !errors.As(err, &ce) || ce.Details["op"] != "ingest" || ce.Details["slug"] != "sgw" || ce.Status != 409 {
		t.Fatalf("a lint on a busy wiki = %v, want IsWikiBusy with the ingest's details", err)
	}
	if _, err := c.WikiDelete("sgw"); !client.IsWikiBusy(err) {
		t.Fatalf("deleting a busy wiki = %v, want IsWikiBusy", err)
	}
	// but a QUERY is answered at once: it takes no lock
	if answer, err := c.WikiQuery("sgw", "while busy?", 10*time.Second); err != nil || answer != "answer to: query" {
		t.Fatalf("a query during the ingest = %q, %v, want an answer (no lock)", answer, err)
	}

	close(gate)
	deadline := time.Now().Add(8 * time.Second)
	for {
		l, _ := c.WikiList()
		if len(l) == 1 && l[0].Status == "ready" && l[0].Op == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the wiki never became ready: %+v", l)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// a clash: a different file under a name that exists
	_, err = c.WikiIngest("sgw", client.WikiSources{Files: []client.WikiFile{{Name: "ctx/n.md", Content: "CHANGED"}, {Name: "fresh.md", Content: "F"}}})
	paths, ok := client.IsWikiConflict(err)
	if !ok || len(paths) != 1 || paths[0] != "ctx/n.md" {
		t.Fatalf("a clash: %v (paths %v)", err, paths)
	}
	// hold the lint's maintainer: an instant one would already be done when the 202 is built, and
	// the test would race on whether it sees "busy"
	lintGate := make(chan struct{})
	ai.mu.Lock()
	ai.gate[maintainer.OpLint] = lintGate
	ai.mu.Unlock()
	if acc, err := c.WikiLint("sgw", "check the links"); err != nil || acc.Op != "lint" || acc.Status != "busy" {
		t.Fatalf("WikiLint = %+v, %v", acc, err)
	}
	close(lintGate)
	for deadline = time.Now().Add(8 * time.Second); ; {
		if l, _ := c.WikiList(); l[0].Status == "ready" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the lint never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st, err := c.WikiDelete("sgw"); err != nil || st.Code != "deleted" {
		t.Fatalf("WikiDelete = %+v, %v", st, err)
	}
	if _, err := c.WikiDelete("sgw"); err == nil || client.IsWikiBusy(err) {
		t.Fatalf("deleting twice = %v, want a not-found", err)
	}
}

// A query that times out on the server is a 504 *client.Error the caller can classify.
func TestAQueryTimeoutIsAClassifiableError(t *testing.T) {
	st, _ := wiki.OpenFS(filepath.Join(t.TempDir(), "harness-wiki"))
	ai := &holdAI{gate: map[maintainer.Op]chan struct{}{}}
	svc := service.New(st, ai)
	srv := httptest.NewServer(api.New(svc))
	t.Cleanup(func() { srv.Close(); svc.Close(time.Second); st.Close() })
	c := client.New(srv.URL)
	c.WikiCreate("w", "d")
	ai.mu.Lock()
	ai.gate[maintainer.OpQuery] = make(chan struct{})
	ai.mu.Unlock()
	_, err := c.WikiQuery("w", "slow", time.Second)
	var ce *client.Error
	if !errors.As(err, &ce) || ce.Status != 504 {
		t.Fatalf("a timed-out query = %v, want a 504 *client.Error", err)
	}
}

func TestNewAcceptsAHostPortOrAURL(t *testing.T) {
	srv := httptest.NewServer(api.New(service.New(mustStore(t), &holdAI{})))
	t.Cleanup(srv.Close)
	for _, addr := range []string{srv.URL, srv.URL[len("http://"):], srv.URL + "/"} {
		if _, err := client.New(addr).WikiList(); err != nil {
			t.Errorf("client.New(%q): %v", addr, err)
		}
	}
}

func mustStore(t *testing.T) *wiki.FSStore {
	st, err := wiki.OpenFS(filepath.Join(t.TempDir(), "harness-wiki"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}
