package wiki

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// ingestEnv is a store with one wiki and an open ingest operation, plus a scratch
// directory for source files outside the wiki.
type ingestEnv struct {
	t   *testing.T
	s   *FSStore
	op  Op
	dir string // the wiki folder
	raw string // <dir>/raw
	src string // a scratch folder outside the wiki
}

func newIngestEnv(t *testing.T) *ingestEnv {
	t.Helper()
	s := newStore(t)
	mustInit(t, s, "w", "d")
	op, err := s.Begin("w", "ingest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { op.Done(true) })
	dir := filepath.Join(s.Root(), "w")
	return &ingestEnv{t: t, s: s, op: op, dir: dir, raw: filepath.Join(dir, "raw"), src: t.TempDir()}
}

func (e *ingestEnv) write(rel, content string) string {
	e.t.Helper()
	p := filepath.Join(e.src, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *ingestEnv) read(rel string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.raw, filepath.FromSlash(rel)))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

// tree lists every file under raw/ (relative, "/"-separated, sorted).
func (e *ingestEnv) tree() []string {
	e.t.Helper()
	out := []string{}
	filepath.WalkDir(e.raw, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(e.raw, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// dirs lists every folder under raw/.
func (e *ingestEnv) dirs() []string {
	e.t.Helper()
	out := []string{}
	filepath.WalkDir(e.raw, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != e.raw {
			rel, _ := filepath.Rel(e.raw, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func (e *ingestEnv) ingest(in Sources) (IngestResult, error) {
	e.t.Helper()
	return e.op.AddSources(in)
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestIngestASingleFile(t *testing.T) {
	e := newIngestEnv(t)
	p := e.write("paper.pdf", "PDFDATA")
	res, err := e.ingest(Sources{Paths: []string{p}})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, res.Ingested, []string{"paper.pdf"})
	if e.read("paper.pdf") != "PDFDATA" {
		t.Fatal("content differs")
	}
	eq(t, e.tree(), []string{"paper.pdf"})
}

func TestIngestAGlob(t *testing.T) {
	e := newIngestEnv(t)
	e.write("notes/a.md", "A")
	e.write("notes/b.md", "B")
	e.write("notes/c.txt", "C")
	e.write("notes/deep/d.md", "D")
	res, err := e.ingest(Sources{Paths: []string{filepath.Join(e.src, "notes", "*.md")}})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, res.Ingested, []string{"a.md", "b.md"}) // not c.txt, not the nested d.md
	eq(t, e.tree(), []string{"a.md", "b.md"})
}

func TestIngestADoubleStarGlobDescends(t *testing.T) {
	e := newIngestEnv(t)
	e.write("docs/a.md", "A")
	e.write("docs/x/b.md", "B")
	e.write("docs/x/y/c.md", "C")
	e.write("docs/x/y/c.txt", "no")
	e.write("other/d.md", "no")
	res, err := e.ingest(Sources{Paths: []string{filepath.Join(e.src, "docs", "**", "*.md")}})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, res.Ingested, []string{"a.md", "b.md", "c.md"})
}

// Two matches that share a base name would land on the same raw/ file: that is a
// reported clash, never a silent overwrite of one by the other.
func TestIngestAGlobWithTwoFilesOfTheSameNameIsRefused(t *testing.T) {
	e := newIngestEnv(t)
	e.write("docs/a/readme.md", "ONE")
	e.write("docs/b/readme.md", "TWO")
	_, err := e.ingest(Sources{Paths: []string{filepath.Join(e.src, "docs", "**", "readme.md")}})
	if err == nil || !strings.Contains(err.Error(), "readme.md") {
		t.Fatalf("err = %v, want a refusal naming readme.md", err)
	}
	eq(t, e.tree(), nil)
}

func TestIngestAGlobThatMatchesNothingIsAnError(t *testing.T) {
	e := newIngestEnv(t)
	e.write("a.txt", "x")
	for _, pat := range []string{filepath.Join(e.src, "*.md"), filepath.Join(e.src, "nodir", "*.md")} {
		if _, err := e.ingest(Sources{Paths: []string{pat}}); err == nil {
			t.Errorf("glob %q that matches nothing must be an error", pat)
		}
	}
}

// A directory is copied whole, the directory itself included.
func TestIngestAWholeDirectoryKeepsTheDirectory(t *testing.T) {
	e := newIngestEnv(t)
	e.write("ctx/a.md", "A")
	e.write("ctx/sub/b.md", "B")
	e.write("ctx/sub/deeper/c.bin", "C")
	res, err := e.ingest(Sources{Paths: []string{filepath.Join(e.src, "ctx")}})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, res.Ingested, []string{"ctx/a.md", "ctx/sub/b.md", "ctx/sub/deeper/c.bin"})
	eq(t, e.tree(), []string{"ctx/a.md", "ctx/sub/b.md", "ctx/sub/deeper/c.bin"})
	if e.read("ctx/sub/deeper/c.bin") != "C" {
		t.Fatal("nested content differs")
	}
}

func TestIngestInlineFilesCreateFoldersFromTheName(t *testing.T) {
	e := newIngestEnv(t)
	res, err := e.ingest(Sources{Files: []File{
		{Name: "note.md", Content: "N"},
		{Name: "ctx/data.md", Content: "D"},
		{Name: `win\style\f.txt`, Content: "W"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, res.Ingested, []string{"ctx/data.md", "note.md", "win/style/f.txt"})
	if e.read("ctx/data.md") != "D" || e.read("win/style/f.txt") != "W" {
		t.Fatal("inline content differs")
	}
}

func TestIngestMixesPathsAndInlineFiles(t *testing.T) {
	e := newIngestEnv(t)
	p := e.write("a.md", "A")
	res, err := e.ingest(Sources{Paths: []string{p}, Files: []File{{Name: "ctx/b.md", Content: "B"}}})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, res.Ingested, []string{"a.md", "ctx/b.md"})
}

func TestIngestRequiresASource(t *testing.T) {
	e := newIngestEnv(t)
	if _, err := e.ingest(Sources{}); err == nil || !strings.Contains(err.Error(), "nothing to ingest") {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.ingest(Sources{Paths: []string{"   "}}); err == nil {
		t.Fatal("a blank path must be an error")
	}
	if _, err := e.ingest(Sources{Paths: []string{filepath.Join(e.src, "missing.md")}}); err == nil {
		t.Fatal("a missing file must be an error")
	}
}

// Re-ingesting what is already there is fine and changes nothing.
func TestAnIdenticalFileCountsAsAlreadyIngested(t *testing.T) {
	e := newIngestEnv(t)
	p := e.write("a.md", "SAME")
	if _, err := e.ingest(Sources{Paths: []string{p}}); err != nil {
		t.Fatal(err)
	}
	res, err := e.ingest(Sources{Paths: []string{p}, Files: []File{{Name: "b.md", Content: "NEW"}}})
	if err != nil {
		t.Fatalf("re-ingesting an identical file must not fail: %v", err)
	}
	eq(t, res.AlreadyIngested, []string{"a.md"})
	eq(t, res.Ingested, []string{"b.md"})
	// inline identical too
	res, err = e.ingest(Sources{Files: []File{{Name: "b.md", Content: "NEW"}}})
	if err != nil || len(res.Ingested) != 0 || !reflect.DeepEqual(res.AlreadyIngested, []string{"b.md"}) {
		t.Fatalf("res = %+v err = %v", res, err)
	}
}

// All or nothing: one differing file fails the whole ingest, lists EVERY clash, and
// not even the files that did not clash are written.
func TestAClashFailsTheWholeIngestAndWritesNothing(t *testing.T) {
	e := newIngestEnv(t)
	if _, err := e.ingest(Sources{Files: []File{{Name: "one.md", Content: "ORIGINAL1"}, {Name: "two.md", Content: "ORIGINAL2"}}}); err != nil {
		t.Fatal(err)
	}
	before := e.tree()
	dirsBefore := e.dirs()

	_, err := e.ingest(Sources{Files: []File{
		{Name: "one.md", Content: "CHANGED1"},
		{Name: "fresh.md", Content: "FRESH"},
		{Name: "newdir/inner.md", Content: "INNER"},
		{Name: "two.md", Content: "CHANGED2"},
		{Name: "ORIGINAL-same.md", Content: "x"},
	}})
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *ConflictError", err)
	}
	if !reflect.DeepEqual(ce.Paths, []string{"one.md", "two.md"}) {
		t.Fatalf("conflicts = %v, want both clashes listed", ce.Paths)
	}
	eq(t, e.tree(), before)
	eq(t, e.dirs(), dirsBefore) // and no folder was created for a file that was never copied
	if e.read("one.md") != "ORIGINAL1" || e.read("two.md") != "ORIGINAL2" {
		t.Fatal("a refused ingest changed an existing file")
	}
}

// Case-insensitive clashes, both against raw/ and inside one ingest.
func TestCaseOnlyDifferencesAreAClash(t *testing.T) {
	e := newIngestEnv(t)
	_, err := e.ingest(Sources{Files: []File{{Name: "Notes.md", Content: "1"}, {Name: "notes.md", Content: "2"}}})
	if err == nil || !strings.Contains(err.Error(), "letter case") {
		t.Fatalf("err = %v, want a letter-case refusal", err)
	}
	eq(t, e.tree(), nil)
	_, err = e.ingest(Sources{Files: []File{{Name: "same.md", Content: "1"}, {Name: "same.md", Content: "1"}}})
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("err = %v, want a named-twice refusal", err)
	}
}

// Whatever the name, nothing may be written outside raw/.
func TestInlineNamesCanNeverEscapeRaw(t *testing.T) {
	e := newIngestEnv(t)
	for _, name := range []string{"../escape.md", "a/../../escape.md", "/abs.md", `..\escape.md`, "C:/x.md", `\\srv\share\x.md`, "ok/../../x"} {
		if _, err := e.ingest(Sources{Files: []File{{Name: name, Content: "PWNED"}}}); err == nil {
			t.Errorf("name %q was accepted", name)
		}
	}
	for _, leak := range []string{filepath.Join(e.dir, "escape.md"), filepath.Join(e.dir, "..", "escape.md"), filepath.Join(e.s.Root(), "escape.md")} {
		if exists(leak) {
			t.Errorf("a file escaped raw/: %s", leak)
		}
	}
	eq(t, e.tree(), nil)
}

// A symlink INSIDE a directory is skipped and reported — a link to a secret must not be
// carried into the wiki. A symlink the caller names explicitly is followed.
func TestSymlinksInsideADirectoryAreSkippedAndReported(t *testing.T) {
	e := newIngestEnv(t)
	secret := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(secret, []byte("TOP SECRET"), 0o644)
	e.write("ctx/real.md", "REAL")
	if err := os.Symlink(secret, filepath.Join(e.src, "ctx", "link.txt")); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	secretDir := t.TempDir()
	os.WriteFile(filepath.Join(secretDir, "k"), []byte("KEY"), 0o644)
	os.Symlink(secretDir, filepath.Join(e.src, "ctx", "dirlink"))

	res, err := e.ingest(Sources{Paths: []string{filepath.Join(e.src, "ctx")}})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, res.Ingested, []string{"ctx/real.md"})
	eq(t, res.Skipped, []string{"ctx/dirlink: symlink", "ctx/link.txt: symlink"})
	eq(t, e.tree(), []string{"ctx/real.md"})
	for _, f := range e.tree() {
		if strings.Contains(e.read(f), "SECRET") || strings.Contains(e.read(f), "KEY") {
			t.Fatalf("the contents of a symlink target were copied into %s", f)
		}
	}
}

func TestAnExplicitSymlinkIsFollowed(t *testing.T) {
	e := newIngestEnv(t)
	target := filepath.Join(t.TempDir(), "real.md")
	os.WriteFile(target, []byte("CONTENT"), 0o644)
	link := filepath.Join(e.src, "link.md")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	res, err := e.ingest(Sources{Paths: []string{link}})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, res.Ingested, []string{"link.md"})
	if e.read("link.md") != "CONTENT" {
		t.Fatal("an explicitly named symlink must be followed")
	}
}

// A destination folder in raw/ that is a symlink (a wiki copied from elsewhere) is
// refused: writing through it would land outside the wiki.
func TestIngestRefusesToWriteThroughASymlinkedRawFolder(t *testing.T) {
	e := newIngestEnv(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(e.raw, "ctx")); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	_, err := e.ingest(Sources{Files: []File{{Name: "ctx/data.md", Content: "LEAK"}}})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v, want a refusal to pass through a symlink", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("a file was written outside the wiki through a symlink: %v", entries)
	}
}

// A source that contains the wiki, or lies inside it, is refused.
func TestAWikiCannotIngestItself(t *testing.T) {
	e := newIngestEnv(t)
	for _, p := range []string{e.dir, e.raw, filepath.Join(e.dir, "AGENTS.md"), filepath.Dir(e.dir), e.s.Root(), filepath.Join(e.dir, "wiki")} {
		if _, err := e.ingest(Sources{Paths: []string{p}}); err == nil || !strings.Contains(err.Error(), "overlaps") {
			t.Errorf("ingesting %s: err = %v, want an overlap refusal", p, err)
		}
	}
	eq(t, e.tree(), nil)
}

func TestIngestLimits(t *testing.T) {
	oldF, oldB := maxIngestFiles, maxIngestBytes
	t.Cleanup(func() { maxIngestFiles, maxIngestBytes = oldF, oldB })

	e := newIngestEnv(t)
	for _, n := range []string{"a", "b", "c"} {
		e.write("many/"+n+".txt", "x")
	}
	maxIngestFiles = 2
	_, err := e.ingest(Sources{Paths: []string{filepath.Join(e.src, "many")}})
	if err == nil || !strings.Contains(err.Error(), "more than 2 files") {
		t.Fatalf("err = %v, want the file-count limit", err)
	}
	eq(t, e.tree(), nil)

	maxIngestFiles, maxIngestBytes = oldF, 5
	_, err = e.ingest(Sources{Files: []File{{Name: "big.txt", Content: "0123456789"}}})
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v, want the size limit", err)
	}
	eq(t, e.tree(), nil)
}

// A failure in the middle of the copy leaves raw/ exactly as it was: the files already
// written and the folders created for them are removed.
func TestAFailureMidCopyRollsEverythingBack(t *testing.T) {
	e := newIngestEnv(t)
	good1 := e.write("src/one.md", "ONE")
	good2 := e.write("src/two.md", "TWO")
	gone := e.write("src/three.md", "THREE")
	// planned fine, but unreadable by copy time: it vanishes between plan and copy
	plan, _, err := e.op.(*fsOp).plan(Sources{Paths: []string{good1, good2, gone}, Files: []File{{Name: "newdir/inline.md", Content: "IN"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	// the order matters for the test to mean something: the failing source must come
	// after at least one successful copy and after a folder was created.
	sort.SliceStable(plan, func(i, j int) bool {
		rank := func(s source) int {
			switch {
			case strings.HasPrefix(s.rel, "newdir/"):
				return 0
			case s.from == gone:
				return 2
			}
			return 1
		}
		return rank(plan[i]) < rank(plan[j])
	})
	made, err := copyAll(e.raw, plan)
	if err == nil {
		t.Fatal("copyAll should have failed on the vanished source")
	}
	if len(made.files) == 0 || len(made.dirs) == 0 {
		t.Fatalf("test setup: expected some files and a folder to exist before the failure, got %+v", made)
	}
	removeCreated(made)
	eq(t, e.tree(), nil)
	eq(t, e.dirs(), nil)
	// and no temp files were left in raw/
	filepath.WalkDir(e.raw, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), ".ingest-") {
			t.Errorf("a temp file was left behind: %s", p)
		}
		return nil
	})
}

// A destination that appears between the plan and the copy is never overwritten.
func TestCopyNeverOverwritesAFileThatAppearedMeanwhile(t *testing.T) {
	e := newIngestEnv(t)
	dest := filepath.Join(e.raw, "race.md")
	os.WriteFile(dest, []byte("THE OTHER WRITER"), 0o644)
	_, err := copyAll(e.raw, []source{{rel: "race.md", content: []byte("MINE"), size: 4}})
	if err == nil || !strings.Contains(err.Error(), "nothing was overwritten") {
		t.Fatalf("err = %v, want a refusal to overwrite", err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "THE OTHER WRITER" {
		t.Fatal("an existing file was overwritten")
	}
	for _, e := range mustReadDir(t, filepath.Dir(dest)) {
		if strings.HasPrefix(e, ".ingest-") {
			t.Errorf("a temp file was left behind: %s", e)
		}
	}
}

func mustReadDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// Copying a file never changes its source, and the copy is not executable.
func TestIngestLeavesTheSourceAloneAndCopiesAreNotExecutable(t *testing.T) {
	e := newIngestEnv(t)
	p := e.write("tool.sh", "#!/bin/sh\necho hi\n")
	os.Chmod(p, 0o755)
	before, _ := os.ReadFile(p)
	if _, err := e.ingest(Sources{Paths: []string{p}}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("the source file was modified")
	}
	if st, err := os.Stat(filepath.Join(e.raw, "tool.sh")); err != nil || st.Mode().Perm()&0o111 != 0 {
		t.Fatalf("the copy must not be executable: %v %v", st, err)
	}
}

// An existing path containing glob characters is taken literally.
func TestAPathWithGlobCharactersThatExistsIsTakenLiterally(t *testing.T) {
	e := newIngestEnv(t)
	p := e.write("notes [draft].md", "DRAFT")
	e.write("notes d.md", "decoy")
	res, err := e.ingest(Sources{Paths: []string{p}})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, res.Ingested, []string{"notes [draft].md"})
}

func TestIngestSizeAndHashDecideIdentity(t *testing.T) {
	e := newIngestEnv(t)
	if _, err := e.ingest(Sources{Files: []File{{Name: "f.md", Content: "AAAA"}}}); err != nil {
		t.Fatal(err)
	}
	// same size, different bytes: a conflict, not "already ingested"
	_, err := e.ingest(Sources{Files: []File{{Name: "f.md", Content: "BBBB"}}})
	if !errors.As(err, new(*ConflictError)) {
		t.Fatalf("same size but different content = %v, want a conflict", err)
	}
	// a file where a folder is needed (or the reverse) is a conflict, never a panic
	_, err = e.ingest(Sources{Files: []File{{Name: "f.md/sub.md", Content: "x"}}})
	if err == nil {
		t.Fatal("writing under a regular file must fail")
	}
	eq(t, e.tree(), []string{"f.md"})
}

// The property that matters, through the public entry point: when a copy fails part way,
// AddSources itself leaves raw/ exactly as it was — the files already written AND the
// folders it created for them — and reports the error.
func TestAddSourcesRollsBackWhenACopyFailsPartWay(t *testing.T) {
	e := newIngestEnv(t)
	if _, err := e.ingest(Sources{Files: []File{{Name: "existing/keep.md", Content: "KEEP"}}}); err != nil {
		t.Fatal(err)
	}
	treeBefore, dirsBefore := e.tree(), e.dirs()

	t.Cleanup(func() { copyHook = nil })
	copyHook = func(rel string) error {
		if rel == "ctx/third.md" {
			return errors.New("injected: disk full")
		}
		return nil
	}
	_, err := e.ingest(Sources{Files: []File{
		{Name: "first.md", Content: "1"},
		{Name: "newdir/deep/second.md", Content: "2"},
		{Name: "ctx/third.md", Content: "3"},
		{Name: "existing/added.md", Content: "4"},
	}})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the injected failure", err)
	}
	eq(t, e.tree(), treeBefore)
	eq(t, e.dirs(), dirsBefore)
	if e.read("existing/keep.md") != "KEEP" {
		t.Fatal("the rollback damaged a file that was already there")
	}
	// and the wiki is usable afterwards: the same ingest, no failure, now succeeds
	copyHook = nil
	res, err := e.ingest(Sources{Files: []File{{Name: "first.md", Content: "1"}, {Name: "newdir/deep/second.md", Content: "2"}}})
	if err != nil || len(res.Ingested) != 2 {
		t.Fatalf("a retry after the rollback: %+v %v", res, err)
	}
}

// safeDest is the SECOND barrier behind ValidRelPath: it must refuse to leave raw/ on its
// own, for any input, so the confinement does not depend on the first filter being right.
func TestSafeDestConfinesToRawByItself(t *testing.T) {
	raw := filepath.Join(t.TempDir(), "raw")
	if err := os.MkdirAll(raw, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"../x", "a/../../x", "a/b/../../../x", "..", "../raw-sibling/x", "ok/../../x"} {
		if got, err := safeDest(raw, rel); err == nil {
			t.Errorf("safeDest(%q) = %q, want a refusal: it leaves raw/", rel, got)
		} else if !errors.Is(err, ErrInvalidPath) || !strings.Contains(err.Error(), "leaves raw/") {
			t.Errorf("safeDest(%q) = %v, want ErrInvalidPath 'leaves raw/'", rel, err)
		}
	}
	for _, rel := range []string{"a", "a/b", "a/b/c.md", "a/../b"} { // a/../b stays inside raw/
		got, err := safeDest(raw, rel)
		if err != nil {
			t.Errorf("safeDest(%q) = %v, want ok", rel, err)
			continue
		}
		if !within(got, raw) {
			t.Errorf("safeDest(%q) = %q is not inside raw/", rel, got)
		}
	}
}

func TestWithin(t *testing.T) {
	for _, c := range []struct {
		path, dir string
		want      bool
	}{
		{"/a/b", "/a", true},
		{"/a", "/a", true},
		{"/a/b/c", "/a/b", true},
		{"/ab", "/a", false}, // a sibling sharing a prefix is not inside
		{"/a", "/a/b", false},
		{"/x", "/a", false},
		{"/a/../x", "/a", false},
	} {
		if got := within(filepath.FromSlash(c.path), filepath.FromSlash(c.dir)); got != c.want {
			t.Errorf("within(%q, %q) = %v, want %v", c.path, c.dir, got, c.want)
		}
	}
}

// A request that is wrong in itself is ErrInvalidInput — typed, so a caller (the HTTP API)
// answers 400 by what the error IS, never by matching its wording.
func TestARequestThatIsWrongInItselfIsErrInvalidInput(t *testing.T) {
	oldF := maxIngestFiles
	t.Cleanup(func() { maxIngestFiles = oldF })
	e := newIngestEnv(t)
	e.write("a/readme.md", "1")
	e.write("b/readme.md", "2")
	e.write("many/x.txt", "x")
	e.write("many/y.txt", "y")
	self := e.dir

	cases := map[string]Sources{
		"nothing to ingest":       {},
		"an empty path":           {Paths: []string{"  "}},
		"a missing file":          {Paths: []string{filepath.Join(e.src, "missing.md")}},
		"a glob with no match":    {Paths: []string{filepath.Join(e.src, "*.nope")}},
		"a glob in a missing dir": {Paths: []string{filepath.Join(e.src, "nodir", "*.md")}},
		"the wiki itself":         {Paths: []string{self}},
		"the same name twice":     {Files: []File{{Name: "x.md", Content: "1"}, {Name: "x.md", Content: "1"}}},
		"a case-only collision":   {Files: []File{{Name: "X.md", Content: "1"}, {Name: "x.md", Content: "2"}}},
		"two files, one name":     {Paths: []string{filepath.Join(e.src, "**", "readme.md")}},
	}
	for name, in := range cases {
		if _, err := e.ingest(in); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: err = %v, want ErrInvalidInput", name, err)
		}
	}
	maxIngestFiles = 1
	if _, err := e.ingest(Sources{Paths: []string{filepath.Join(e.src, "many")}}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("too many files: err = %v, want ErrInvalidInput", err)
	}
	maxIngestFiles = oldF
	// a bad inline NAME is the path error, also a 400
	if _, err := e.ingest(Sources{Files: []File{{Name: "../x", Content: "x"}}}); !errors.Is(err, ErrInvalidPath) {
		t.Errorf("a bad name: err = %v, want ErrInvalidPath", err)
	}
	// and the other kinds must NOT be mistaken for it
	e.ingest(Sources{Files: []File{{Name: "keep.md", Content: "A"}}})
	if _, err := e.ingest(Sources{Files: []File{{Name: "keep.md", Content: "B"}}}); errors.Is(err, ErrInvalidInput) || !errors.As(err, new(*ConflictError)) {
		t.Errorf("a clash: err = %v, want a ConflictError and not ErrInvalidInput", err)
	}
}
