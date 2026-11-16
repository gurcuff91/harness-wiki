package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gurcuff91/harness-wiki/maintainer"
	"github.com/gurcuff91/harness-wiki/wiki"
)

// fakeAI records every run and lets a test hold, fail, panic or answer it.
type fakeAI struct {
	mu      sync.Mutex
	runs    []maintainer.Run
	reply   string
	err     error
	gate    chan struct{} // when set, every run waits for it
	started chan struct{}
	panics  bool
}

func newFakeAI() *fakeAI { return &fakeAI{reply: "the report", started: make(chan struct{}, 64)} }

func (f *fakeAI) Execute(ctx context.Context, run maintainer.Run) (string, error) {
	f.mu.Lock()
	f.runs = append(f.runs, run)
	gate, err, reply, panics := f.gate, f.err, f.reply, f.panics
	f.mu.Unlock()
	f.started <- struct{}{}
	if panics {
		panic("boom")
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "partial", ctx.Err()
		}
	}
	return reply, err
}

func (f *fakeAI) last() maintainer.Run {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs[len(f.runs)-1]
}
func (f *fakeAI) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.runs) }
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

func newService(t *testing.T) (*Service, *fakeAI, *wiki.FSStore) {
	t.Helper()
	st, err := wiki.OpenFS(filepath.Join(t.TempDir(), "harness-wiki"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ai := newFakeAI()
	svc := New(st, ai)
	t.Cleanup(func() { svc.Close(2 * time.Second) })
	return svc, ai, st
}

func waitStatus(t *testing.T, st wiki.Store, slug string, want wiki.Status) wiki.Info {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for {
		info, err := st.Get(slug)
		if err == nil && info.Status == want {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never became %s (last %+v, %v)", slug, want, info, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func create(t *testing.T, svc *Service, slug string) {
	t.Helper()
	if _, err := svc.Create(context.Background(), slug, "d", ""); err != nil {
		t.Fatal(err)
	}
}

func files(names ...string) wiki.Sources {
	var s wiki.Sources
	for _, n := range names {
		s.Files = append(s.Files, wiki.File{Name: n, Content: "content of " + n})
	}
	return s
}

// ── create ──────────────────────────────────────────────────────────────────

func TestCreateMakesTheWikiThenAdaptsItsSchema(t *testing.T) {
	svc, ai, st := newService(t)
	c, err := svc.Create(context.Background(), "sgw", "Chrysler SGW research", "p/m")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Adapted || c.AdaptedError != "" || c.Info.Slug != "sgw" || c.Info.Status != wiki.StatusReady {
		t.Fatalf("created = %+v", c)
	}
	r := ai.last()
	if r.Op != maintainer.OpInit || r.Slug != "sgw" || r.Model != "p/m" || !strings.Contains(r.Prompt, "Chrysler SGW research") || !strings.Contains(r.Prompt, "Do not create pages") {
		t.Fatalf("run = %+v", r)
	}
	if info, _ := st.Get("sgw"); info.Status != wiki.StatusReady {
		t.Errorf("after create the wiki is %s, want ready", info.Status)
	}
	if exists(filepath.Join(st.Root(), "sgw", ".pending")) {
		t.Error("init left a pending marker")
	}
}

// The wiki exists whatever happens to the AI part: init is not atomic with the model.
func TestCreateSurvivesAFailedAdaptation(t *testing.T) {
	svc, ai, st := newService(t)
	ai.err = errors.New("model refused the request")
	c, err := svc.Create(context.Background(), "w", "d", "")
	if err != nil {
		t.Fatalf("a failed adaptation must not fail the create: %v", err)
	}
	if c.Adapted || !strings.Contains(c.AdaptedError, "model refused the request") {
		t.Fatalf("created = %+v, want not adapted, with the reason", c)
	}
	if _, err := st.Get("w"); err != nil {
		t.Fatalf("the wiki does not exist: %v", err)
	}
}

func TestCreateValidatesBeforeTouchingAnything(t *testing.T) {
	svc, ai, st := newService(t)
	for _, c := range []struct{ slug, desc string }{{"ok", ""}, {"ok", "   "}} {
		if _, err := svc.Create(context.Background(), c.slug, c.desc, ""); !errors.Is(err, wiki.ErrInvalidInput) {
			t.Errorf("Create(%q, %q) = %v, want ErrInvalidInput", c.slug, c.desc, err)
		}
	}
	for _, slug := range []string{"Bad Slug", "../x", "con", ""} {
		if _, err := svc.Create(context.Background(), slug, "d", ""); !errors.Is(err, wiki.ErrInvalidSlug) {
			t.Errorf("Create(%q) = %v, want ErrInvalidSlug", slug, err)
		}
	}
	if _, err := svc.Create(context.Background(), "dup", "d", ""); err != nil {
		t.Fatal(err)
	}
	n := ai.count()
	if _, err := svc.Create(context.Background(), "dup", "d2", ""); !errors.Is(err, wiki.ErrExists) {
		t.Errorf("a second create = %v, want ErrExists", err)
	}
	if ai.count() != n {
		t.Error("the maintainer ran for a create that was refused")
	}
	if list, _ := st.List(); len(list) != 1 {
		t.Errorf("wikis = %+v, want only dup", list)
	}
}

func TestCreateStopsTheAdaptationWhenTheCallerGoesAway(t *testing.T) {
	svc, ai, _ := newService(t)
	ai.gate = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Created, 1)
	go func() { c, _ := svc.Create(ctx, "w", "d", ""); done <- c }()
	ai.waitStarted(t, 1)
	cancel()
	select {
	case c := <-done:
		if c.Adapted || c.AdaptedError == "" {
			t.Errorf("created = %+v, want not adapted", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the create never returned after its caller went away")
	}
}

// ── ingest ──────────────────────────────────────────────────────────────────

func TestIngestReturnsAtOnceAndHoldsTheWikiUntilTheMaintainerFinishes(t *testing.T) {
	svc, ai, st := newService(t)
	create(t, svc, "w")
	ai.gate = make(chan struct{})
	t0 := time.Now()
	acc, err := svc.Ingest("w", files("a.md", "ctx/b.md"), "focus on the CAN bus", "m/x")
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(t0) > 2*time.Second {
		t.Errorf("Ingest took %v: it must return once the sources are copied", time.Since(t0))
	}
	if strings.Join(acc.Ingest.Ingested, ",") != "a.md,ctx/b.md" {
		t.Fatalf("ingested = %v", acc.Ingest.Ingested)
	}
	ai.waitStarted(t, 2) // the init run, then the ingest run
	r := ai.last()
	if r.Op != maintainer.OpIngest || r.Model != "m/x" || r.Dir == "" {
		t.Fatalf("run = %+v", r)
	}
	for _, want := range []string{"a.md", "ctx/b.md", "focus on the CAN bus", "Ingest workflow", "Never modify raw/"} {
		if !strings.Contains(r.Prompt, want) {
			t.Errorf("the ingest prompt lacks %q:\n%s", want, r.Prompt)
		}
	}
	// while the maintainer works the wiki is busy with ingest, and others are refused
	if info, _ := st.Get("w"); info.Status != wiki.StatusBusy || info.Op != "ingest" {
		t.Fatalf("during the ingest: %+v", info)
	}
	if _, err := svc.Lint("w", "", ""); !wiki.IsBusy(err) {
		t.Errorf("a lint on a busy wiki = %v, want busy", err)
	}
	if _, err := svc.Ingest("w", files("c.md"), "", ""); !wiki.IsBusy(err) {
		t.Errorf("a second ingest = %v, want busy", err)
	}
	if err := svc.Delete("w"); !wiki.IsBusy(err) {
		t.Errorf("a delete on a busy wiki = %v, want busy", err)
	}
	close(ai.gate)
	waitStatus(t, st, "w", wiki.StatusReady)
	if exists(filepath.Join(st.Root(), "w", ".pending")) {
		t.Error("a finished ingest left its pending marker")
	}
}

func TestAFailedOrPanickingMaintainerLeavesTheWikiIncomplete(t *testing.T) {
	for name, set := range map[string]func(*fakeAI){
		"error":  func(f *fakeAI) { f.err = errors.New("provider down") },
		"panic":  func(f *fakeAI) { f.panics = true },
		"cancel": func(f *fakeAI) { f.err = context.Canceled },
	} {
		t.Run(name, func(t *testing.T) {
			svc, ai, st := newService(t)
			create(t, svc, "w")
			set(ai)
			if _, err := svc.Ingest("w", files("a.md"), "", ""); err != nil {
				t.Fatal(err)
			}
			info := waitStatus(t, st, "w", wiki.StatusIncomplete)
			if info.Op != "ingest" {
				t.Errorf("incomplete op = %q, want ingest", info.Op)
			}
			if exists(filepath.Join(st.Root(), "w", ".lock")) {
				t.Error("the lock was not released")
			}
		})
	}
}

// A refused ingest means NOTHING happened: no maintainer, no lock, no marker.
func TestARefusedIngestLeavesTheWikiExactlyAsItWas(t *testing.T) {
	svc, ai, st := newService(t)
	create(t, svc, "w")
	if _, err := svc.Ingest("w", files("keep.md"), "", ""); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, st, "w", wiki.StatusReady)
	runs := ai.count()

	cases := map[string]struct {
		src  wiki.Sources
		want any
	}{
		"nothing":        {wiki.Sources{}, wiki.ErrInvalidInput},
		"a bad name":     {files("../escape.md"), wiki.ErrInvalidPath},
		"a missing path": {wiki.Sources{Paths: []string{"/definitely/not/here"}}, wiki.ErrInvalidInput},
		"a clash":        {wiki.Sources{Files: []wiki.File{{Name: "keep.md", Content: "DIFFERENT"}, {Name: "new.md", Content: "x"}}}, new(*wiki.ConflictError)},
	}
	for name, c := range cases {
		_, err := svc.Ingest("w", c.src, "", "")
		switch want := c.want.(type) {
		case error:
			if !errors.Is(err, want) {
				t.Errorf("%s: %v, want %v", name, err, want)
			}
		case **wiki.ConflictError:
			if !errors.As(err, want) {
				t.Errorf("%s: %v, want a ConflictError", name, err)
			}
		}
	}
	if ai.count() != runs {
		t.Error("the maintainer ran for an ingest that was refused")
	}
	if info, _ := st.Get("w"); info.Status != wiki.StatusReady {
		t.Errorf("a refused ingest left the wiki %s, want ready", info.Status)
	}
	root := st.Root()
	for _, f := range []string{".lock", ".pending", "raw/new.md"} {
		if exists(filepath.Join(root, "w", filepath.FromSlash(f))) {
			t.Errorf("a refused ingest left %s", f)
		}
	}
	if _, err := svc.Ingest("nope", files("a.md"), "", ""); !errors.Is(err, wiki.ErrNotFound) {
		t.Errorf("an unknown wiki: %v", err)
	}
}

func TestIngestIsToldAboutAnEarlierInterruptedOperation(t *testing.T) {
	svc, ai, st := newService(t)
	create(t, svc, "w")
	ai.err = errors.New("crash")
	svc.Ingest("w", files("a.md"), "", "")
	waitStatus(t, st, "w", wiki.StatusIncomplete)
	ai.mu.Lock()
	ai.err = nil
	ai.mu.Unlock()

	acc, err := svc.Lint("w", "", "")
	if err != nil || !acc.Interrupted {
		t.Fatalf("Lint after a crash = %+v, %v; want Interrupted", acc, err)
	}
	waitStatus(t, st, "w", wiki.StatusReady)
	for _, want := range []string{"did not finish cleanly", "verify its coherence"} {
		if !strings.Contains(ai.last().Prompt, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, ai.last().Prompt)
		}
	}
	// and a clean wiki is told nothing of the kind
	svc.Lint("w", "", "")
	waitStatus(t, st, "w", wiki.StatusReady)
	if strings.Contains(ai.last().Prompt, "did not finish cleanly") {
		t.Error("a clean wiki's prompt carries the interruption note")
	}
}

// ── lint ────────────────────────────────────────────────────────────────────

func TestLintRunsInTheBackgroundWithItsGuidance(t *testing.T) {
	svc, ai, st := newService(t)
	create(t, svc, "w")
	ai.gate = make(chan struct{})
	acc, err := svc.Lint("w", "only the Chrysler pages", "")
	if err != nil || acc.Slug != "w" {
		t.Fatalf("%+v %v", acc, err)
	}
	ai.waitStarted(t, 2)
	if r := ai.last(); r.Op != maintainer.OpLint || !strings.Contains(r.Prompt, "only the Chrysler pages") || !strings.Contains(r.Prompt, "Lint workflow") {
		t.Fatalf("run = %+v", r)
	}
	if info, _ := st.Get("w"); info.Status != wiki.StatusBusy || info.Op != "lint" {
		t.Fatalf("during the lint: %+v", info)
	}
	close(ai.gate)
	waitStatus(t, st, "w", wiki.StatusReady)
	if _, err := svc.Lint("nope", "", ""); !errors.Is(err, wiki.ErrNotFound) {
		t.Errorf("an unknown wiki: %v", err)
	}
}

// ── query: no lock, concurrent ─────────────────────────────────────────────

func TestQueryAnswersWithoutTakingTheLock(t *testing.T) {
	svc, ai, st := newService(t)
	create(t, svc, "w")
	out, err := svc.Query(context.Background(), "w", "how does it authenticate?", 0, "m/y")
	if err != nil || out != "the report" {
		t.Fatalf("%q %v", out, err)
	}
	r := ai.last()
	if r.Op != maintainer.OpQuery || r.Model != "m/y" || !strings.Contains(r.Prompt, "how does it authenticate?") || !strings.Contains(r.Prompt, "index.md") {
		t.Fatalf("run = %+v", r)
	}
	if info, _ := st.Get("w"); info.Status != wiki.StatusReady {
		t.Errorf("after a query the wiki is %s, want ready", info.Status)
	}
}

func TestManyQueriesRunAtOnceEvenDuringAnIngest(t *testing.T) {
	svc, ai, st := newService(t)
	create(t, svc, "w")
	ingestGate := make(chan struct{})
	ai.gate = ingestGate
	if _, err := svc.Ingest("w", files("a.md"), "", ""); err != nil {
		t.Fatal(err)
	}
	ai.waitStarted(t, 2)
	// from here on queries must not be held by the ingest's gate
	queryGate := make(chan struct{})
	ai.mu.Lock()
	ai.gate = queryGate
	ai.mu.Unlock()

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Query(context.Background(), "w", "q", 10*time.Second, "")
			errs <- err
		}()
	}
	ai.waitStarted(t, n) // all n reached the maintainer at the same time
	if info, _ := st.Get("w"); info.Status != wiki.StatusBusy || info.Op != "ingest" {
		t.Errorf("with %d queries in flight the wiki is %+v, want still busy with the ingest alone", n, info)
	}
	close(queryGate)
	close(ingestGate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a query failed: %v", err)
		}
	}
	waitStatus(t, st, "w", wiki.StatusReady)
}

func TestQueryValidatesAndReportsAnUnknownWiki(t *testing.T) {
	svc, ai, _ := newService(t)
	create(t, svc, "w")
	n := ai.count()
	for _, q := range []string{"", "   "} {
		if _, err := svc.Query(context.Background(), "w", q, 0, ""); !errors.Is(err, wiki.ErrInvalidInput) {
			t.Errorf("Query(%q) = %v, want ErrInvalidInput", q, err)
		}
	}
	if _, err := svc.Query(context.Background(), "nope", "q", 0, ""); !errors.Is(err, wiki.ErrNotFound) {
		t.Errorf("an unknown wiki: %v", err)
	}
	if _, err := svc.Query(context.Background(), "../w", "q", 0, ""); !errors.Is(err, wiki.ErrInvalidSlug) {
		t.Errorf("a bad slug: %v", err)
	}
	if ai.count() != n {
		t.Error("the maintainer ran for a query that was refused")
	}
}

func TestQueryTimeoutIsATypedErrorAndDiscardsThePartialAnswer(t *testing.T) {
	svc, ai, st := newService(t)
	create(t, svc, "w")
	ai.gate = make(chan struct{})
	t0 := time.Now()
	out, err := svc.Query(context.Background(), "w", "q", 1*time.Second, "")
	var te *TimeoutError
	if !errors.As(err, &te) || te.After != time.Second {
		t.Fatalf("err = %v, want *TimeoutError after 1s", err)
	}
	if out != "" {
		t.Errorf("a timed-out query returned %q: the partial answer must be discarded", out)
	}
	if time.Since(t0) > 4*time.Second {
		t.Errorf("took %v for a 1 s timeout", time.Since(t0))
	}
	if info, _ := st.Get("w"); info.Status != wiki.StatusReady {
		t.Errorf("a timed-out query left the wiki %s, want ready", info.Status)
	}
}

func TestQueryStopsWhenTheCallerGoesAway(t *testing.T) {
	svc, ai, _ := newService(t)
	create(t, svc, "w")
	ai.gate = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := svc.Query(ctx, "w", "q", time.Minute, ""); done <- err }()
	ai.waitStarted(t, 2)
	cancel()
	select {
	case err := <-done:
		var te *TimeoutError
		if err == nil || errors.As(err, &te) {
			t.Errorf("err = %v: a caller going away is not a timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the query kept running after its caller went away")
	}
}

func TestClampTimeout(t *testing.T) {
	for _, c := range []struct{ in, def, want time.Duration }{
		{0, QueryTimeout, QueryTimeout},
		{-1, InitTimeout, InitTimeout},
		{45 * time.Second, QueryTimeout, 45 * time.Second},
		{24 * time.Hour, QueryTimeout, MaxTimeout},
	} {
		if got := ClampTimeout(c.in, c.def); got != c.want {
			t.Errorf("ClampTimeout(%v, %v) = %v, want %v", c.in, c.def, got, c.want)
		}
	}
	if QueryTimeout <= InitTimeout {
		t.Error("a query is the slowest synchronous operation: its default must be the longest")
	}
}

// ── list, delete, shutdown ──────────────────────────────────────────────────

func TestListIsNeverNilAndDeleteWorks(t *testing.T) {
	svc, _, st := newService(t)
	if list, err := svc.List(); err != nil || list == nil || len(list) != 0 {
		t.Fatalf("List on a fresh service = %#v, %v", list, err)
	}
	create(t, svc, "a")
	create(t, svc, "b")
	if list, _ := svc.List(); len(list) != 2 || list[0].Slug != "a" {
		t.Fatalf("list = %+v", list)
	}
	if err := svc.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete("a"); !errors.Is(err, wiki.ErrNotFound) {
		t.Errorf("deleting twice: %v", err)
	}
	if list, _ := st.List(); len(list) != 1 || list[0].Slug != "b" {
		t.Fatalf("after delete: %+v", list)
	}
}

// Close waits for background work, and refuses new background work once closing.
func TestCloseWaitsForBackgroundWorkAndRefusesNewWork(t *testing.T) {
	svc, ai, st := newService(t)
	create(t, svc, "w")
	ai.gate = make(chan struct{})
	svc.Ingest("w", files("a.md"), "", "")
	ai.waitStarted(t, 2)

	closed := make(chan struct{})
	go func() { svc.Close(5 * time.Second); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned while a background ingest was still running")
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := svc.Ingest("w", files("b.md"), "", ""); err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Errorf("an ingest during shutdown: %v, want a shutting-down refusal", err)
	}
	if _, err := svc.Lint("w", "", ""); err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Errorf("a lint during shutdown: %v, want a shutting-down refusal", err)
	}
	close(ai.gate)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close never returned after the work finished")
	}
	waitStatus(t, st, "w", wiki.StatusReady)
}

func TestCloseGivesUpAfterTheGracePeriod(t *testing.T) {
	svc, ai, _ := newService(t)
	create(t, svc, "w")
	ai.gate = make(chan struct{}) // never released
	svc.Ingest("w", files("a.md"), "", "")
	ai.waitStarted(t, 2)
	t0 := time.Now()
	svc.Close(200 * time.Millisecond)
	if el := time.Since(t0); el > 2*time.Second {
		t.Errorf("Close took %v: it must give up after its grace period", el)
	}
	close(ai.gate)
}

func TestMergeEndsWithEitherParentAndLeaksNothing(t *testing.T) {
	a, cancelA := context.WithCancel(context.Background())
	b, cancelB := context.WithCancel(context.Background())
	defer cancelA()
	defer cancelB()
	m := merge(a, b)
	if m.Err() != nil {
		t.Fatal("a fresh merged context is already done")
	}
	cancelB()
	select {
	case <-m.Done():
	case <-time.After(time.Second):
		t.Fatal("cancelling the second parent did not end the merged context")
	}
	long, stop := context.WithCancel(context.Background())
	defer stop()
	for i := 0; i < 100; i++ {
		p, c := context.WithCancel(context.Background())
		mm := merge(p, long)
		c()
		<-mm.Done()
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// When the wiki's lock is lost (this process was suspended past the heartbeat and another took the
// wiki over) the operation's context ends, and the background maintainer must STOP — not keep
// writing into a wiki it no longer owns. (A fake store hands the service an operation whose
// context the test can cancel, standing in for the heartbeat noticing.)
func TestABackgroundMaintainerStopsWhenTheWikiLockIsLost(t *testing.T) {
	ai := newFakeAI()
	ai.gate = make(chan struct{}) // never released: only cancellation can end the run
	st := &lockLossStore{FSStore: mustStore(t), cancels: make(chan context.CancelFunc, 4)}
	svc := New(st, ai)
	t.Cleanup(func() { svc.Close(time.Second) })
	if _, err := st.Init("w", "d"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Lint("w", "", ""); err != nil {
		t.Fatal(err)
	}
	ai.waitStarted(t, 1)
	select {
	case cancel := <-st.cancels:
		cancel() // the lock is lost
	case <-time.After(3 * time.Second):
		t.Fatal("the service never began an operation")
	}
	waitStatus(t, st.FSStore, "w", wiki.StatusIncomplete) // stopped, lock released, marker kept
}

// lockLossStore wraps the real store so a test can end an operation's context from outside.
type lockLossStore struct {
	*wiki.FSStore
	cancels chan context.CancelFunc
}

func (s *lockLossStore) Begin(slug, op string) (wiki.Op, error) {
	o, err := s.FSStore.Begin(slug, op)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(o.Context())
	s.cancels <- cancel
	return &ctxOp{Op: o, ctx: ctx}, nil
}

type ctxOp struct {
	wiki.Op
	ctx context.Context
}

func (o *ctxOp) Context() context.Context { return o.ctx }

func mustStore(t *testing.T) *wiki.FSStore {
	t.Helper()
	st, err := wiki.OpenFS(filepath.Join(t.TempDir(), "harness-wiki"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// An ingest with nothing to ingest is refused BEFORE anything is touched: it must not take the
// wiki's lock (a stray empty request must not make the wiki busy) and the maintainer must not run.
func TestAnEmptyIngestIsRefusedBeforeTakingTheLock(t *testing.T) {
	svc, ai, st := newService(t)
	create(t, svc, "w")
	n := ai.count()
	// hold the wiki with a lint: if the empty ingest tried to take the lock it would get "busy",
	// not the input error — which is the observable difference
	ai.gate = make(chan struct{})
	if _, err := svc.Lint("w", "", ""); err != nil {
		t.Fatal(err)
	}
	ai.waitStarted(t, 2)
	_, err := svc.Ingest("w", wiki.Sources{}, "", "")
	if !errors.Is(err, wiki.ErrInvalidInput) || wiki.IsBusy(err) {
		t.Fatalf("an empty ingest on a busy wiki = %v, want the input error (checked before the lock)", err)
	}
	close(ai.gate)
	waitStatus(t, st, "w", wiki.StatusReady)
	if ai.count() != n+1 { // only the lint ran
		t.Errorf("the maintainer ran %d times, want %d (just the lint)", ai.count()-n, 1)
	}
}

// Create reports the wiki as it is AFTER the adaptation released its lock: ready, not the busy
// state it was in while the maintainer worked.
func TestCreateReportsTheWikiAsReadyNotAsBusy(t *testing.T) {
	svc, _, _ := newService(t)
	c, err := svc.Create(context.Background(), "w", "d", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Info.Status != wiki.StatusReady || c.Info.Op != "" || !c.Info.Since.IsZero() {
		t.Fatalf("Create reported %+v, want ready with no op", c.Info)
	}
	// and when the adaptation FAILS the wiki is reported ready too
	svc2, ai2, _ := newService(t)
	ai2.err = errors.New("boom")
	c, _ = svc2.Create(context.Background(), "w", "d", "")
	if c.Info.Status != wiki.StatusReady {
		t.Fatalf("after a failed adaptation Create reported %+v, want ready", c.Info)
	}
}

// The prompt tells the maintainer which sources are NOT new, so it does not redo them.
func TestThePromptFlagsAlreadyIngestedSourcesAsNotNew(t *testing.T) {
	p := Prompt(maintainer.OpIngest, PromptArgs{Ingested: []string{"new.md"}, AlreadyIngested: []string{"old.md"}})
	for _, want := range []string{"new.md", "old.md", "are NOT new", "do not re-ingest"} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(Prompt(maintainer.OpIngest, PromptArgs{Ingested: []string{"a.md"}}), "NOT new") {
		t.Error("the not-new paragraph appears although nothing was already ingested")
	}
}
