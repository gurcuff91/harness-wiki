package wiki

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newStore opens a store on a fresh temp root. The root is a subfolder that does NOT
// exist yet, so the lazy-creation behaviour is exercised.
func newStore(t *testing.T) *FSStore {
	t.Helper()
	s, err := OpenFS(filepath.Join(t.TempDir(), "wikis"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustInit(t *testing.T, s *FSStore, slug, desc string) Info {
	t.Helper()
	info, err := s.Init(slug, desc)
	if err != nil {
		t.Fatalf("Init(%q): %v", slug, err)
	}
	return info
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// The default store root is ./harness-wiki in the working directory, and merely opening a store
// creates nothing: the folder appears with the first Init.
func TestOpenFSIsLazyAndDefaultsToAFolderInTheWorkingDirectory(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	// a HOME that holds a .harness-wiki / .harness must make no difference at all
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	os.MkdirAll(filepath.Join(home, ".harness", "wiki"), 0o755)

	s, err := OpenFS("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// t.TempDir may sit behind a symlink (/var → /private/var on macOS); compare resolved paths
	want, _ := filepath.EvalSymlinks(cwd)
	want = filepath.Join(want, "harness-wiki")
	got, _ := filepath.EvalSymlinks(filepath.Dir(s.Root()))
	if filepath.Join(got, filepath.Base(s.Root())) != want {
		t.Fatalf("Root() = %q, want %q (a folder called harness-wiki in the working directory)", s.Root(), want)
	}
	if exists(filepath.Join(cwd, "harness-wiki")) {
		t.Fatal("opening the store created ./harness-wiki — it must touch nothing until the first Init")
	}
	if list, err := s.List(); err != nil || len(list) != 0 {
		t.Fatalf("List on a store that was never used = %v, %v; want an empty list and no error", list, err)
	}
	if exists(filepath.Join(cwd, "harness-wiki")) {
		t.Fatal("List created the store root")
	}
	mustInit(t, s, "first", "the first wiki")
	if !exists(filepath.Join(cwd, "harness-wiki", "first", "wiki.json")) {
		t.Fatal("the first Init must create ./harness-wiki/first/")
	}
	// the home directory was never consulted nor written
	if entries, _ := os.ReadDir(home); len(entries) != 1 || entries[0].Name() != ".harness" {
		t.Errorf("the home directory changed: %v", entries)
	}
}

// Starting the process in another directory puts the wikis in another place: that is the whole
// way to choose where they live.
func TestTheDefaultStoreFollowsTheWorkingDirectory(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	t.Chdir(a)
	sa, _ := OpenFS("")
	mustInit(t, sa, "in-a", "d")
	t.Chdir(b)
	sb, _ := OpenFS("")
	if list, _ := sb.List(); len(list) != 0 {
		t.Fatalf("a store opened in another directory sees %v", list)
	}
	mustInit(t, sb, "in-b", "d")
	for dir, slug := range map[string]string{a: "in-a", b: "in-b"} {
		if !exists(filepath.Join(dir, "harness-wiki", slug, "wiki.json")) {
			t.Errorf("%s is not under %s/harness-wiki", slug, dir)
		}
	}
}

func TestInitCreatesTheWholeLayout(t *testing.T) {
	s := newStore(t)
	before := time.Now().Add(-time.Second)
	info := mustInit(t, s, "chrysler-sgw", "Research on the Chrysler SGW bypass")
	if info.Slug != "chrysler-sgw" || info.Status != StatusReady || info.CreatedAt.Before(before) {
		t.Fatalf("Info = %+v", info)
	}
	dir := filepath.Join(s.Root(), "chrysler-sgw")
	for _, p := range []string{"wiki.json", "AGENTS.md", "raw", filepath.Join("wiki", "index.md"), filepath.Join("wiki", "log.md")} {
		if !exists(filepath.Join(dir, p)) {
			t.Errorf("Init did not create %s", p)
		}
	}
	for _, p := range []string{".lock", ".pending"} {
		if exists(filepath.Join(dir, p)) {
			t.Errorf("Init left %s behind", p)
		}
	}
	var m manifest
	b, _ := os.ReadFile(filepath.Join(dir, "wiki.json"))
	if err := json.Unmarshal(b, &m); err != nil || m.Slug != "chrysler-sgw" || m.Description != "Research on the Chrysler SGW bypass" || m.CreatedAt.IsZero() {
		t.Fatalf("manifest = %+v (%v)", m, err)
	}
	agents, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	for _, want := range []string{"# Wiki: chrysler-sgw", "Research on the Chrysler SGW bypass", "raw/", "index.md", "log.md", "## [YYYY-MM-DD] op | title", "Ingest", "Query", "Lint"} {
		if !strings.Contains(string(agents), want) {
			t.Errorf("AGENTS.md lacks %q", want)
		}
	}
	log, _ := os.ReadFile(filepath.Join(dir, "wiki", "log.md"))
	if !strings.Contains(string(log), "] init | chrysler-sgw") {
		t.Errorf("log.md has no init entry in the documented format:\n%s", log)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("a temp file was left behind: %s", e.Name())
		}
	}
}

func TestInitValidatesBeforeTouchingTheDisk(t *testing.T) {
	s := newStore(t)
	for _, slug := range []string{"", "Bad", "../x", "a/b", "con", "-a"} {
		if _, err := s.Init(slug, "desc"); !errors.Is(err, ErrInvalidSlug) {
			t.Errorf("Init(%q) = %v, want ErrInvalidSlug", slug, err)
		}
	}
	if _, err := s.Init("ok", "   "); err == nil || !strings.Contains(err.Error(), "description") {
		t.Errorf("Init with a blank description = %v, want a description error", err)
	}
	if exists(s.Root()) {
		t.Error("a rejected Init created the store root")
	}
}

func TestInitNeverOverwrites(t *testing.T) {
	s := newStore(t)
	mustInit(t, s, "w", "original")
	agents := filepath.Join(s.Root(), "w", "AGENTS.md")
	if err := os.WriteFile(agents, []byte("EDITED BY THE MAINTAINER"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Init("w", "a different description"); !errors.Is(err, ErrExists) {
		t.Fatalf("second Init = %v, want ErrExists", err)
	}
	if b, _ := os.ReadFile(agents); string(b) != "EDITED BY THE MAINTAINER" {
		t.Fatal("a second Init overwrote the wiki's schema")
	}
	got, _ := s.Get("w")
	if got.Description != "original" {
		t.Fatalf("the description was changed to %q", got.Description)
	}
}

// A folder that is not a wiki (no manifest) is never adopted or overwritten.
func TestInitRefusesAFolderThatIsNotAWiki(t *testing.T) {
	s := newStore(t)
	dir := filepath.Join(s.Root(), "precious")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "notes.txt")
	os.WriteFile(keep, []byte("user data"), 0o644)
	_, err := s.Init("precious", "x")
	if err == nil || errors.Is(err, ErrExists) || !strings.Contains(err.Error(), "not a wiki") {
		t.Fatalf("err = %v, want a 'not a wiki' error distinct from ErrExists", err)
	}
	if b, _ := os.ReadFile(keep); string(b) != "user data" {
		t.Fatal("Init damaged a folder it did not own")
	}
	if exists(filepath.Join(dir, "wiki.json")) {
		t.Fatal("Init adopted a foreign folder")
	}
}

// Init fails at ANY step: the folder it created is removed, nothing is listed, and the
// slug can be used again. And at the moment the manifest is written every other file
// already exists — the manifest really is last.
func TestAFailedInitLeavesNothingAtEveryStep(t *testing.T) {
	steps := []string{"raw", "wiki", "AGENTS.md", "index.md", "log.md", "wiki.json"}
	for _, failAt := range steps {
		t.Run(failAt, func(t *testing.T) {
			s := newStore(t)
			t.Cleanup(func() { initStepHook = nil })
			initStepHook = func(path string) error {
				if filepath.Base(path) == failAt {
					return errors.New("injected failure")
				}
				return nil
			}
			if _, err := s.Init("w", "d"); err == nil || !strings.Contains(err.Error(), "injected failure") {
				t.Fatalf("Init = %v, want the injected failure", err)
			}
			if exists(filepath.Join(s.Root(), "w")) {
				t.Fatalf("a failed Init (at %s) left the wiki folder behind", failAt)
			}
			if list, _ := s.List(); len(list) != 0 {
				t.Fatalf("List = %+v after a failed Init", list)
			}
			initStepHook = nil
			if _, err := s.Init("w", "d"); err != nil {
				t.Fatalf("the slug could not be reused after a failed Init: %v", err)
			}
		})
	}
}

func TestTheManifestIsTheLastThingInitWrites(t *testing.T) {
	s := newStore(t)
	t.Cleanup(func() { initStepHook = nil })
	var atManifest []string
	initStepHook = func(path string) error {
		if filepath.Base(path) == "wiki.json" {
			dir := filepath.Dir(path)
			for _, p := range []string{"raw", "wiki", "AGENTS.md", "wiki/index.md", "wiki/log.md"} {
				if exists(filepath.Join(dir, filepath.FromSlash(p))) {
					atManifest = append(atManifest, p)
				}
			}
			if exists(path) {
				t.Error("the manifest existed before its step")
			}
		}
		return nil
	}
	mustInit(t, s, "w", "d")
	if len(atManifest) != 5 {
		t.Fatalf("when the manifest was written only %v existed — it must come last", atManifest)
	}
}

// A folder with no manifest (what a crash before the last step would leave) is invisible.
func TestAFolderWithoutAManifestIsNotAWiki(t *testing.T) {
	s := newStore(t)
	dir := filepath.Join(s.Root(), "halfway")
	os.MkdirAll(filepath.Join(dir, "raw"), 0o755)
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("x"), 0o644)
	if list, _ := s.List(); len(list) != 0 {
		t.Fatalf("List = %+v: a folder with no manifest must not show up", list)
	}
	if _, err := s.Get("halfway"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get = %v, want ErrNotFound", err)
	}
}

func TestListIgnoresWhatIsNotAWiki(t *testing.T) {
	s := newStore(t)
	mustInit(t, s, "beta", "second")
	mustInit(t, s, "alpha", "first")
	root := s.Root()
	// a plain file, a folder with no manifest, a folder with a corrupt manifest, a
	// folder whose name is not a valid slug (even with a manifest), and a loose file.
	os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(root, "nomanifest"), 0o755)
	os.MkdirAll(filepath.Join(root, "corrupt"), 0o755)
	os.WriteFile(filepath.Join(root, "corrupt", "wiki.json"), []byte("{not json"), 0o644)
	os.MkdirAll(filepath.Join(root, "Not_A_Slug"), 0o755)
	os.WriteFile(filepath.Join(root, "Not_A_Slug", "wiki.json"), []byte(`{"slug":"x","description":"d"}`), 0o644)

	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, w := range list {
		got = append(got, w.Slug)
	}
	if strings.Join(got, ",") != "alpha,beta" {
		t.Fatalf("List = %v, want [alpha beta] (sorted, and ignoring everything that is not a wiki)", got)
	}
	if list[0].Description != "first" || list[0].Status != StatusReady {
		t.Errorf("alpha = %+v", list[0])
	}
}

// Identity is the FOLDER name: a wiki copied and renamed keeps working under its new
// folder, whatever the slug written inside its manifest says.
func TestFolderNameIsTheIdentity(t *testing.T) {
	s := newStore(t)
	mustInit(t, s, "original", "d")
	if err := os.Rename(filepath.Join(s.Root(), "original"), filepath.Join(s.Root(), "renamed")); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List()
	if len(list) != 1 || list[0].Slug != "renamed" {
		t.Fatalf("List = %+v, want the wiki under its folder name", list)
	}
	if _, err := s.Get("renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("original"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(original) = %v, want ErrNotFound", err)
	}
}

func TestListSkipsASymlinkedFolder(t *testing.T) {
	s := newStore(t)
	mustInit(t, s, "real", "d")
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "wiki.json"), []byte(`{"slug":"link","description":"outside"}`), 0o644)
	if err := os.Symlink(outside, filepath.Join(s.Root(), "link")); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	list, _ := s.List()
	if len(list) != 1 || list[0].Slug != "real" {
		t.Fatalf("List = %+v: a symlink out of the store must not be listed", list)
	}
	if _, err := s.Get("link"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get through a symlink = %v, want ErrNotFound", err)
	}
	if _, err := s.Begin("link", "query"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Begin through a symlink = %v, want ErrNotFound", err)
	}
}

func TestStatusIsComputedLive(t *testing.T) {
	fastLock(t)
	s := newStore(t)
	mustInit(t, s, "w", "d")
	status := func() Info {
		t.Helper()
		i, err := s.Get("w")
		if err != nil {
			t.Fatal(err)
		}
		return i
	}

	if i := status(); i.Status != StatusReady || i.Op != "" || !i.Since.IsZero() {
		t.Fatalf("fresh wiki = %+v, want ready with no op", i)
	}

	op, err := s.Begin("w", "ingest")
	if err != nil {
		t.Fatal(err)
	}
	i := status()
	if i.Status != StatusBusy || i.Op != "ingest" || i.Since.IsZero() {
		t.Fatalf("during an ingest = %+v, want busy/ingest/since — even though a pending marker ALSO exists (busy wins)", i)
	}
	op.Done(true)
	if i := status(); i.Status != StatusReady {
		t.Fatalf("after a clean ingest = %+v, want ready", i)
	}

	// An interrupted ingest: lock gone, marker stays.
	op, _ = s.Begin("w", "ingest")
	op.Done(false)
	i = status()
	if i.Status != StatusIncomplete || i.Op != "ingest" || i.Since.IsZero() {
		t.Fatalf("after a failed ingest = %+v, want incomplete/ingest", i)
	}
	// and it shows in List too
	list, _ := s.List()
	if list[0].Status != StatusIncomplete {
		t.Fatalf("List = %+v", list[0])
	}
}

// If the process dies mid-ingest the wiki is busy until the heartbeat expires, then
// incomplete — never stuck busy.
func TestACrashedIngestBecomesIncompleteAfterTheLockExpires(t *testing.T) {
	fastLock(t)
	s := newStore(t)
	mustInit(t, s, "w", "d")
	dir := filepath.Join(s.Root(), "w")
	// what a killed process leaves: a lock nobody renews, and the marker
	if err := createExclusive(filepath.Join(dir, lockFile), lockInfo{Op: "ingest", StartedAt: time.Now().UTC(), Token: "dead"}); err != nil {
		t.Fatal(err)
	}
	writePending(dir, "ingest")
	if i, _ := s.Get("w"); i.Status != StatusBusy {
		t.Fatalf("right after the crash = %+v, want busy", i)
	}
	time.Sleep(2 * staleAfter)
	if i, _ := s.Get("w"); i.Status != StatusIncomplete {
		t.Fatalf("after the lock expired = %+v, want incomplete", i)
	}
	// and the next operation is told about the interrupted one, then takes the wiki
	op, err := s.Begin("w", "lint")
	if err != nil {
		t.Fatalf("an expired lock must not block: %v", err)
	}
	defer op.Done(true)
	prev, since, ok := op.Interrupted()
	if !ok || prev != "ingest" || since.IsZero() {
		t.Fatalf("Interrupted() = %q %v %v, want the earlier ingest", prev, since, ok)
	}
}

func TestBeginWritesPendingOnlyForIngestAndLint(t *testing.T) {
	s := newStore(t)
	mustInit(t, s, "w", "d")
	dir := filepath.Join(s.Root(), "w")
	for op, want := range map[string]bool{"ingest": true, "lint": true, "query": false, "init": false} {
		o, err := s.Begin("w", op)
		if err != nil {
			t.Fatalf("Begin(%s): %v", op, err)
		}
		if got := exists(filepath.Join(dir, ".pending")); got != want {
			t.Errorf("%s: pending marker present = %v, want %v", op, got, want)
		}
		o.Done(true)
		if exists(filepath.Join(dir, ".pending")) || exists(filepath.Join(dir, ".lock")) {
			t.Errorf("%s: Done(true) left .pending or .lock behind", op)
		}
	}
}

// Query must never turn a wiki "incomplete", even when it is cut short.
func TestAFailedQueryLeavesNoPendingMarker(t *testing.T) {
	s := newStore(t)
	mustInit(t, s, "w", "d")
	o, _ := s.Begin("w", "query")
	o.Done(false)
	if i, _ := s.Get("w"); i.Status != StatusReady {
		t.Fatalf("after a failed query = %+v, want ready", i)
	}
}

func TestBeginIsRefusedWhileBusyAndForUnknownWikis(t *testing.T) {
	s := newStore(t)
	mustInit(t, s, "w", "d")
	o, err := s.Begin("w", "ingest")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin("w", "query"); !IsBusy(err) {
		t.Fatalf("second Begin = %v, want busy", err)
	}
	o.Done(true)
	if _, err := s.Begin("nope", "query"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Begin on an unknown wiki = %v, want ErrNotFound", err)
	}
	if _, err := s.Begin("w", "  "); err == nil {
		t.Fatal("Begin with a blank operation name must fail")
	}
	if _, err := s.Begin("../w", "query"); !errors.Is(err, ErrInvalidSlug) {
		t.Fatalf("Begin with a bad slug = %v, want ErrInvalidSlug", err)
	}
}

// Abort means "nothing happened": the wiki is exactly as it was — including an OLDER
// interrupted marker, which must survive.
func TestAbortRestoresTheWikiAsItWas(t *testing.T) {
	s := newStore(t)
	mustInit(t, s, "w", "d")
	dir := filepath.Join(s.Root(), "w")

	o, _ := s.Begin("w", "ingest")
	o.Abort()
	if exists(filepath.Join(dir, ".pending")) || exists(filepath.Join(dir, ".lock")) {
		t.Fatal("Abort on a clean wiki left a marker or the lock")
	}
	if i, _ := s.Get("w"); i.Status != StatusReady {
		t.Fatalf("status = %+v, want ready", i)
	}

	// an older interrupted ingest exists; a new ingest that aborts must not erase it
	o, _ = s.Begin("w", "ingest")
	o.Done(false)
	older, _ := readPending(dir)
	o, _ = s.Begin("w", "ingest")
	if _, _, ok := o.Interrupted(); !ok {
		t.Fatal("the new operation was not told about the interrupted one")
	}
	o.Abort()
	again, ok := readPending(dir)
	if !ok || again.Op != older.Op || !again.StartedAt.Equal(older.StartedAt) {
		t.Fatalf("Abort lost the older interrupted marker: before %+v, after %+v (%v)", older, again, ok)
	}
	if i, _ := s.Get("w"); i.Status != StatusIncomplete {
		t.Fatalf("status = %+v, want still incomplete", i)
	}
}

func TestDoneAndAbortAreOnceOnly(t *testing.T) {
	s := newStore(t)
	mustInit(t, s, "w", "d")
	o, _ := s.Begin("w", "ingest")
	o.Done(true)
	o.Done(false) // must not resurrect the marker
	o.Abort()
	if exists(filepath.Join(s.Root(), "w", ".pending")) {
		t.Fatal("a second Done/Abort changed the wiki")
	}
	// and the lock really is free for the next operation
	o2, err := s.Begin("w", "query")
	if err != nil {
		t.Fatal(err)
	}
	o2.Done(true)
}

func TestDeleteRemovesTheManifestFirstAndTheWholeWiki(t *testing.T) {
	s := newStore(t)
	mustInit(t, s, "gone", "d")
	mustInit(t, s, "stays", "d")
	dir := filepath.Join(s.Root(), "gone")
	os.WriteFile(filepath.Join(dir, "raw", "a.txt"), []byte("src"), 0o644)
	os.MkdirAll(filepath.Join(dir, "wiki", "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "wiki", "sub", "p.md"), []byte("page"), 0o644)

	if err := s.Delete("gone"); err != nil {
		t.Fatal(err)
	}
	if exists(dir) {
		t.Fatal("the wiki folder survived Delete")
	}
	list, _ := s.List()
	if len(list) != 1 || list[0].Slug != "stays" {
		t.Fatalf("List = %+v, want only the other wiki", list)
	}
	if err := s.Delete("gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting twice = %v, want ErrNotFound", err)
	}
}

// Delete removes the manifest BEFORE the rest: if removing the contents fails part way,
// the wiki is already invisible, so what is left is an ignored folder and never a
// listed wiki that is broken.
func TestAHalfDeletedWikiIsInvisible(t *testing.T) {
	s := newStore(t)
	mustInit(t, s, "w", "d")
	dir := filepath.Join(s.Root(), "w")
	t.Cleanup(func() { removeAllHook = nil })
	removeAllHook = func(string) error { return errors.New("injected: the disk refused") }

	err := s.Delete("w")
	if err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("Delete = %v, want the injected failure", err)
	}
	if !exists(dir) {
		t.Fatal("test setup: the folder should still be there (the removal was refused)")
	}
	if exists(filepath.Join(dir, "wiki.json")) {
		t.Fatal("the manifest survived: it must be removed BEFORE the rest, or a failed Delete leaves a listed, broken wiki")
	}
	if list, _ := s.List(); len(list) != 0 {
		t.Fatalf("List = %+v: a half-deleted wiki must already be invisible", list)
	}
	if _, err := s.Get("w"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get = %v, want ErrNotFound", err)
	}
	if exists(filepath.Join(dir, ".lock")) {
		t.Fatal("a failed Delete left the lock behind")
	}
	if err := s.Delete("w"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete of a manifest-less leftover = %v, want ErrNotFound (it is not ours to remove)", err)
	}
	if !exists(dir) {
		t.Fatal("Delete removed a folder that is not a wiki")
	}
}

func TestDeleteRefusesABusyWiki(t *testing.T) {
	s := newStore(t)
	mustInit(t, s, "w", "d")
	o, _ := s.Begin("w", "ingest")
	err := s.Delete("w")
	if !IsBusy(err) {
		t.Fatalf("Delete while an ingest runs = %v, want busy", err)
	}
	if _, err := s.Get("w"); err != nil {
		t.Fatalf("a refused Delete damaged the wiki: %v", err)
	}
	o.Done(true)
	if err := s.Delete("w"); err != nil {
		t.Fatalf("Delete after the ingest: %v", err)
	}
}

// Delete never follows a symlink out of the store and never touches a folder that is
// not a wiki.
func TestDeleteNeverFollowsASymlinkOrRemovesForeignFolders(t *testing.T) {
	s := newStore(t)
	mustInit(t, s, "real", "d")
	outside := t.TempDir()
	victim := filepath.Join(outside, "precious.txt")
	os.WriteFile(victim, []byte("do not delete"), 0o644)
	os.WriteFile(filepath.Join(outside, "wiki.json"), []byte(`{"slug":"evil","description":"x"}`), 0o644)
	if err := os.Symlink(outside, filepath.Join(s.Root(), "evil")); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	if err := s.Delete("evil"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete through a symlink = %v, want ErrNotFound", err)
	}
	if !exists(victim) {
		t.Fatal("Delete followed a symlink and removed files outside the store")
	}
	for _, slug := range []string{"..", "../real", "real/../real", "/", ""} {
		if err := s.Delete(slug); !errors.Is(err, ErrInvalidSlug) {
			t.Errorf("Delete(%q) = %v, want ErrInvalidSlug", slug, err)
		}
	}
	if !exists(filepath.Join(s.Root(), "real")) {
		t.Fatal("a rejected Delete removed a wiki")
	}
}

// Closing the store releases the locks of operations still running, so a normal
// shutdown does not leave a wiki "busy" until the heartbeat expires.
func TestCloseReleasesOpenOperations(t *testing.T) {
	s := newStore(t)
	mustInit(t, s, "w", "d")
	o, err := s.Begin("w", "ingest")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(s.Root(), "w", ".lock")) {
		t.Fatal("Close left the wiki locked")
	}
	if i, _ := s.Get("w"); i.Status != StatusIncomplete {
		t.Fatalf("after Close with an ingest running = %+v, want incomplete (the marker stays)", i)
	}
	if o.Context().Err() == nil {
		t.Error("the operation's context must end when its lock is released")
	}
	if _, err := s.Begin("w", "query"); err == nil {
		t.Error("a closed store must not start operations")
	}
	o.Done(true) // late Done after Close is harmless
}

// Two stores on the same root (two harness processes) see each other's locks.
func TestTwoStoresOnTheSameRootShareTheLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), "wikis")
	a, _ := OpenFS(root)
	b, _ := OpenFS(root)
	defer a.Close()
	defer b.Close()
	if _, err := a.Init("w", "d"); err != nil {
		t.Fatal(err)
	}
	o, err := a.Begin("w", "ingest")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Begin("w", "query"); !IsBusy(err) {
		t.Fatalf("the second process's Begin = %v, want busy", err)
	}
	if i, _ := b.Get("w"); i.Status != StatusBusy {
		t.Fatalf("the second process sees %+v, want busy", i)
	}
	if err := b.Delete("w"); !IsBusy(err) {
		t.Fatalf("the second process's Delete = %v, want busy", err)
	}
	o.Done(true)
	if _, err := b.Begin("w", "query"); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

// Begin works only on a real wiki: a folder with no manifest (a half-made or half-deleted one,
// or something that was never a wiki) is "not found", and no lock or marker is written in it.
func TestBeginRefusesAFolderWithoutAManifest(t *testing.T) {
	s := newStore(t)
	dir := filepath.Join(s.Root(), "notawiki")
	if err := os.MkdirAll(filepath.Join(dir, "raw"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"ingest", "lint", "query"} {
		if _, err := s.Begin("notawiki", op); !errors.Is(err, ErrNotFound) {
			t.Errorf("Begin(%s) on a folder with no manifest = %v, want ErrNotFound", op, err)
		}
	}
	for _, f := range []string{".lock", ".pending"} {
		if exists(filepath.Join(dir, f)) {
			t.Errorf("a refused Begin left %s in a folder that is not a wiki", f)
		}
	}
	// a corrupt manifest is the same
	os.WriteFile(filepath.Join(dir, "wiki.json"), []byte("{not json"), 0o644)
	if _, err := s.Begin("notawiki", "query"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Begin with a corrupt manifest = %v, want ErrNotFound", err)
	}
}

// ── View: reading without the lock ──────────────────────────────────────────

// A View takes no lock and writes nothing: opening one on a wiki leaves it exactly as it was, and
// any number can be open at once.
func TestViewTakesNoLockAndWritesNothing(t *testing.T) {
	s := newStore(t)
	mustInit(t, s, "w", "d")
	dir := filepath.Join(s.Root(), "w")

	views := make([]View, 0, 20)
	for i := 0; i < 20; i++ {
		v, err := s.View("w")
		if err != nil {
			t.Fatalf("view %d: %v", i, err)
		}
		if v.Slug() != "w" || v.Dir() != dir {
			t.Fatalf("view = %q %q, want w %q", v.Slug(), v.Dir(), dir)
		}
		views = append(views, v)
	}
	for _, f := range []string{".lock", ".pending"} {
		if exists(filepath.Join(dir, f)) {
			t.Errorf("opening views created %s", f)
		}
	}
	if i, _ := s.Get("w"); i.Status != StatusReady {
		t.Errorf("a wiki with 20 open views is %s, want ready (a read is not an operation)", i.Status)
	}
	_ = views
}

// The whole point: a view opens on a wiki that an ingest, a lint or a delete is holding — it never
// fails with busy — and an operation can still begin while views are open.
func TestViewWorksWhileTheWikiIsBusyAndDoesNotBlockOperations(t *testing.T) {
	s := newStore(t)
	mustInit(t, s, "w", "d")

	for _, op := range []string{"ingest", "lint", "init"} {
		o, err := s.Begin("w", op)
		if err != nil {
			t.Fatalf("Begin(%s): %v", op, err)
		}
		v, err := s.View("w")
		if err != nil {
			t.Fatalf("View during %s = %v, want it to open (a query takes no lock)", op, err)
		}
		if IsBusy(err) {
			t.Errorf("View reported busy during %s", op)
		}
		if _, _, interrupted := v.Interrupted(); interrupted {
			t.Errorf("during a LIVE %s the view reports an interrupted operation: a running ingest is not an interrupted one", op)
		}
		o.Done(true)
	}
	// and the reverse: with views open, an ingest still begins and delete still works
	if _, err := s.View("w"); err != nil {
		t.Fatal(err)
	}
	o, err := s.Begin("w", "ingest")
	if err != nil {
		t.Fatalf("an open view blocked an ingest: %v", err)
	}
	o.Done(true)
	if err := s.Delete("w"); err != nil {
		t.Fatalf("an open view blocked a delete: %v", err)
	}
}

// Two queries and an ingest, all at once, none waiting for another.
func TestConcurrentViewsAndAnIngest(t *testing.T) {
	s := newStore(t)
	mustInit(t, s, "w", "d")
	o, err := s.Begin("w", "ingest")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.View("w"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a concurrent view failed: %v", err)
	}
	o.Done(true)
}

func TestViewRefusesWhatIsNotAWiki(t *testing.T) {
	s := newStore(t)
	mustInit(t, s, "real", "d")
	for _, slug := range []string{"nope", "../real", "Bad Slug", "", "con"} {
		if _, err := s.View(slug); err == nil {
			t.Errorf("View(%q) opened", slug)
		}
	}
	if _, err := s.View("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown wiki: %v, want ErrNotFound", err)
	}
	if _, err := s.View("../real"); !errors.Is(err, ErrInvalidSlug) {
		t.Errorf("a path-like slug: %v, want ErrInvalidSlug", err)
	}
	// a folder with no manifest, and one with a corrupt manifest
	os.MkdirAll(filepath.Join(s.Root(), "bare"), 0o755)
	if _, err := s.View("bare"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a folder with no manifest: %v, want ErrNotFound", err)
	}
	os.WriteFile(filepath.Join(s.Root(), "bare", "wiki.json"), []byte("{no"), 0o644)
	if _, err := s.View("bare"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a corrupt manifest: %v, want ErrNotFound", err)
	}
	// a symlinked folder is not a wiki
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "wiki.json"), []byte(`{"slug":"link","description":"x"}`), 0o644)
	if err := os.Symlink(outside, filepath.Join(s.Root(), "link")); err == nil {
		if _, err := s.View("link"); !errors.Is(err, ErrNotFound) {
			t.Errorf("a symlinked folder: %v, want ErrNotFound", err)
		}
	}
}

// A view reports an interrupted operation only when nobody holds the wiki: a marker with no live
// lock. The same marker WITH a live lock is an ingest running right now.
func TestViewReportsOnlyAnInterruptedOperation(t *testing.T) {
	fastLock(t)
	s := newStore(t)
	mustInit(t, s, "w", "d")
	dir := filepath.Join(s.Root(), "w")

	v, _ := s.View("w")
	if _, _, ok := v.Interrupted(); ok {
		t.Fatal("a clean wiki reports an interrupted operation")
	}
	// what a killed process leaves: a marker, no lock
	if err := writePending(dir, "ingest"); err != nil {
		t.Fatal(err)
	}
	v, _ = s.View("w")
	op, since, ok := v.Interrupted()
	if !ok || op != "ingest" || since.IsZero() {
		t.Fatalf("Interrupted() = %q %v %v, want the earlier ingest", op, since, ok)
	}
	// the same marker with a LIVE lock is a running ingest, not an interrupted one
	l, err := acquireLock("w", dir, "ingest")
	if err != nil {
		t.Fatal(err)
	}
	v, _ = s.View("w")
	if _, _, ok := v.Interrupted(); ok {
		t.Error("a marker with a live lock was reported as interrupted: that is a running ingest")
	}
	l.release()
	// and once the lock is released (the owner crashed) it is interrupted again
	v, _ = s.View("w")
	if _, _, ok := v.Interrupted(); !ok {
		t.Error("after the lock went away the marker was not reported as interrupted")
	}
}

// A View never writes, so closing the store has nothing to release for it, and it survives Close.
func TestViewsDoNotKeepTheStoreOpenOrNeedReleasing(t *testing.T) {
	s := newStore(t)
	mustInit(t, s, "w", "d")
	v, _ := s.View("w")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if v.Dir() == "" {
		t.Fatal("a view lost its folder when the store closed")
	}
}
