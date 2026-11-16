package wiki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// initStepHook, when set by a test, runs before each step of Init with the step's
// path; a non-nil error fails that step. It lets a test prove that a failure part way
// leaves nothing behind. It is nil in production.
var initStepHook func(path string) error

// removeAllHook, when set by a test, replaces the bulk removal at the end of Delete so a
// test can make it fail and prove the wiki is already invisible by then. nil in production.
var removeAllHook func(dir string) error

// manifestFile is each wiki's manifest. Its presence is what makes a folder a wiki.
const manifestFile = "wiki.json"

// manifest is the content of wiki.json. The folder name, not Slug, is the wiki's
// identity — a copied wiki that was renamed still works under its new folder name.
type manifest struct {
	Slug        string    `json:"slug"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

// FSStore is the filesystem Store: one folder per wiki under a root directory.
type FSStore struct {
	root string

	mu     sync.Mutex // guards open
	open   map[*fsOp]struct{}
	closed bool
}

// DefaultRoot is where the default store keeps its wikis: a folder called "harness-wiki" in the
// current working directory — the directory the process was started in. There is no environment
// variable and no home-directory location: to keep wikis elsewhere, start the process elsewhere
// (or pass a root to OpenFS).
func DefaultRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("wiki: resolve working directory: %w", err)
	}
	return filepath.Join(cwd, DefaultFolder), nil
}

// DefaultFolder is the name of the folder the default store lives in.
const DefaultFolder = "harness-wiki"

// OpenFS returns a filesystem store rooted at root ("" = DefaultRoot, ./harness-wiki). It does not
// touch the disk: the root is created by the first Init, so a user who never makes
// a wiki gets no new folder.
func OpenFS(root string) (*FSStore, error) {
	if root == "" {
		var err error
		if root, err = DefaultRoot(); err != nil {
			return nil, err
		}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("wiki: resolve root: %w", err)
	}
	return &FSStore{root: abs, open: map[*fsOp]struct{}{}}, nil
}

// Root is the directory the store keeps its wikis in.
func (s *FSStore) Root() string { return s.root }

// dirOf is the folder of a wiki, after validating the slug.
func (s *FSStore) dirOf(slug string) (string, error) {
	if err := ValidSlug(slug); err != nil {
		return "", err
	}
	return filepath.Join(s.root, slug), nil
}

// readManifest reads dir's manifest. ok is false for a missing or unreadable one.
func readManifest(dir string) (manifest, bool) {
	b, err := os.ReadFile(filepath.Join(dir, manifestFile))
	if err != nil {
		return manifest{}, false
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return manifest{}, false
	}
	return m, true
}

// isRealDir reports whether path is a directory that is not a symbolic link.
func isRealDir(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.IsDir() && st.Mode()&os.ModeSymlink == 0
}

// infoOf builds the Info of the wiki in dir, computing its status from the lock and
// pending files. slug is the folder name.
func infoOf(slug, dir string, m manifest) Info {
	info := Info{Slug: slug, Description: m.Description, CreatedAt: m.CreatedAt, Status: StatusReady}
	if lk, live := lockState(dir); live {
		// busy wins over incomplete: a normal ingest has both a lock and a marker.
		info.Status, info.Op, info.Since = StatusBusy, lk.Op, lk.StartedAt
		return info
	}
	if p, ok := readPending(dir); ok {
		info.Status, info.Op, info.Since = StatusIncomplete, p.Op, p.StartedAt
	}
	return info
}

// Init implements Store.
func (s *FSStore) Init(slug, description string) (Info, error) {
	dir, err := s.dirOf(slug)
	if err != nil {
		return Info{}, err
	}
	description = strings.TrimSpace(description)
	if description == "" {
		return Info{}, errors.New("wiki: a description is required")
	}

	// The root is created lazily, here — never at OpenFS.
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return Info{}, fmt.Errorf("wiki: create store root: %w", err)
	}
	// A non-recursive Mkdir: an existing folder is an error, never silently reused.
	if err := os.Mkdir(dir, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			if _, ok := readManifest(dir); ok {
				return Info{}, fmt.Errorf("%w: %q", ErrExists, slug)
			}
			return Info{}, fmt.Errorf("wiki: folder %s exists but is not a wiki (no wiki.json) — review it or delete it by hand", dir)
		}
		return Info{}, fmt.Errorf("wiki: create %s: %w", dir, err)
	}
	// From here, anything that fails removes what this call created.
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(dir)
		}
	}()

	now := time.Now().UTC()
	steps := []struct {
		path string
		mk   func(string) error
	}{
		{filepath.Join(dir, "raw"), mkdir},
		{filepath.Join(dir, "wiki"), mkdir},
		{filepath.Join(dir, "AGENTS.md"), writer(baseAgentsMD(slug, description))},
		{filepath.Join(dir, "wiki", "index.md"), writer(seedIndex)},
		{filepath.Join(dir, "wiki", "log.md"), writer(seedLog(slug, now))},
	}
	for _, st := range steps {
		if initStepHook != nil {
			if err := initStepHook(st.path); err != nil {
				return Info{}, fmt.Errorf("wiki: create %s: %w", st.path, err)
			}
		}
		if err := st.mk(st.path); err != nil {
			return Info{}, fmt.Errorf("wiki: create %s: %w", st.path, err)
		}
	}
	// The manifest goes LAST and atomically: until it exists the folder is not a wiki
	// (List ignores it), so a crash here leaves nothing visible and half-made.
	if initStepHook != nil {
		if err := initStepHook(filepath.Join(dir, manifestFile)); err != nil {
			return Info{}, fmt.Errorf("wiki: write manifest: %w", err)
		}
	}
	m := manifest{Slug: slug, Description: description, CreatedAt: now}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := writeFileAtomic(filepath.Join(dir, manifestFile), append(b, '\n')); err != nil {
		return Info{}, fmt.Errorf("wiki: write manifest: %w", err)
	}
	ok = true
	return Info{Slug: slug, Description: description, CreatedAt: now, Status: StatusReady}, nil
}

func mkdir(p string) error { return os.Mkdir(p, 0o755) }

func writer(content string) func(string) error {
	return func(p string) error { return os.WriteFile(p, []byte(content), 0o644) }
}

// writeFileAtomic writes content to a temp file next to path and renames it into
// place, so a reader never sees a partial file.
func writeFileAtomic(path string, content []byte) error {
	tmp := fmt.Sprintf("%s.tmp-%d", path, time.Now().UnixNano())
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// List implements Store.
func (s *FSStore) List() ([]Info, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Info{}, nil // nothing created yet
		}
		return nil, fmt.Errorf("wiki: read %s: %w", s.root, err)
	}
	out := []Info{}
	for _, e := range entries {
		name := e.Name()
		// Only folders that could be a wiki, and never a symlink out of the store.
		if ValidSlug(name) != nil {
			continue
		}
		dir := filepath.Join(s.root, name)
		if !isRealDir(dir) {
			continue
		}
		m, ok := readManifest(dir)
		if !ok {
			continue // not a wiki (or a half-made / half-deleted one): ignored
		}
		out = append(out, infoOf(name, dir, m))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

// Get implements Store.
func (s *FSStore) Get(slug string) (Info, error) {
	dir, err := s.dirOf(slug)
	if err != nil {
		return Info{}, err
	}
	if !isRealDir(dir) {
		return Info{}, fmt.Errorf("%w: %q", ErrNotFound, slug)
	}
	m, ok := readManifest(dir)
	if !ok {
		return Info{}, fmt.Errorf("%w: %q", ErrNotFound, slug)
	}
	return infoOf(slug, dir, m), nil
}

// Delete implements Store.
func (s *FSStore) Delete(slug string) error {
	dir, err := s.dirOf(slug)
	if err != nil {
		return err
	}
	// Only a real wiki is removed: a direct child of the root, not a symlink, with a
	// manifest. Anything else is "not found" — Delete never follows a link out of
	// the store and never removes a folder that is not ours.
	if !isRealDir(dir) {
		return fmt.Errorf("%w: %q", ErrNotFound, slug)
	}
	if _, ok := readManifest(dir); !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, slug)
	}
	lock, err := acquireLock(slug, dir, "delete")
	if err != nil {
		return err
	}
	defer lock.release()

	// The manifest first: from this instant the wiki is gone from List, so a failure
	// in the removal below leaves an ignored leftover folder, not a broken wiki.
	if err := os.Remove(filepath.Join(dir, manifestFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("wiki: delete %q: %w", slug, err)
	}
	remove := removeAll
	if removeAllHook != nil {
		remove = removeAllHook
	}
	if err := remove(dir); err != nil {
		return fmt.Errorf("wiki: delete %q: %w", slug, err)
	}
	return nil
}

// removeAll removes dir, retrying briefly: on Windows a folder can be momentarily
// held by an indexer or a virus scanner.
func removeAll(dir string) error {
	var err error
	for i := 0; i < 5; i++ {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return err
}

// Begin implements Store.
func (s *FSStore) Begin(slug, op string) (Op, error) {
	if strings.TrimSpace(op) == "" {
		return nil, errors.New("wiki: an operation name is required")
	}
	dir, err := s.dirOf(slug)
	if err != nil {
		return nil, err
	}
	if !isRealDir(dir) {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, slug)
	}
	if _, ok := readManifest(dir); !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, slug)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("wiki: store is closed")
	}
	s.mu.Unlock()

	lock, err := acquireLock(slug, dir, op)
	if err != nil {
		return nil, err
	}
	o := &fsOp{store: s, slug: slug, dir: dir, op: op, lock: lock}
	// An older interrupted operation is remembered BEFORE this one writes its own
	// marker, so the maintainer can be told about it.
	if p, ok := readPending(dir); ok {
		o.prev, o.hadPrev = p, true
	}
	if op == "ingest" || op == "lint" {
		if err := writePending(dir, op); err != nil {
			lock.release()
			return nil, fmt.Errorf("wiki: write pending marker: %w", err)
		}
		o.wrotePending = true
	}
	s.mu.Lock()
	s.open[o] = struct{}{}
	s.mu.Unlock()
	return o, nil
}

// View implements Store.
func (s *FSStore) View(slug string) (View, error) {
	dir, err := s.dirOf(slug)
	if err != nil {
		return nil, err
	}
	if !isRealDir(dir) {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, slug)
	}
	if _, ok := readManifest(dir); !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, slug)
	}
	v := &fsView{slug: slug, dir: dir}
	// A marker with a LIVE lock is an ingest or lint running right now — not an interrupted
	// one. Only a marker nobody holds is "interrupted".
	if p, ok := readPending(dir); ok {
		if _, live := lockState(dir); !live {
			v.prev, v.hadPrev = p, true
		}
	}
	return v, nil
}

// fsView is the FSStore's View. It holds nothing: no lock, no file, no goroutine.
type fsView struct {
	slug, dir string
	prev      lockInfo
	hadPrev   bool
}

func (v *fsView) Slug() string { return v.slug }
func (v *fsView) Dir() string  { return v.dir }
func (v *fsView) Interrupted() (string, time.Time, bool) {
	return v.prev.Op, v.prev.StartedAt, v.hadPrev
}

// Close implements Store. It cancels every open operation's lock — so a normal
// shutdown does not leave a wiki "busy" until its heartbeat expires — and leaves
// any pending markers in place.
func (s *FSStore) Close() error {
	s.mu.Lock()
	s.closed = true
	ops := make([]*fsOp, 0, len(s.open))
	for o := range s.open {
		ops = append(ops, o)
	}
	s.mu.Unlock()
	for _, o := range ops {
		o.Done(false)
	}
	return nil
}

// fsOp is the FSStore's Op.
type fsOp struct {
	store *FSStore
	slug  string
	dir   string
	op    string
	lock  *heldLock

	prev    lockInfo
	hadPrev bool

	wrotePending bool
	endOnce      sync.Once
}

func (o *fsOp) Slug() string { return o.slug }
func (o *fsOp) Dir() string  { return o.dir }

func (o *fsOp) Interrupted() (string, time.Time, bool) {
	return o.prev.Op, o.prev.StartedAt, o.hadPrev
}

func (o *fsOp) Context() context.Context { return o.lock.ctx }

func (o *fsOp) finish(f func()) {
	o.endOnce.Do(func() {
		f()
		o.lock.release()
		o.store.mu.Lock()
		delete(o.store.open, o)
		o.store.mu.Unlock()
	})
}

func (o *fsOp) Done(ok bool) {
	o.finish(func() {
		if ok {
			removePending(o.dir)
		}
		// !ok: the pending marker (written at Begin for ingest/lint) stays.
	})
}

func (o *fsOp) Abort() {
	o.finish(func() {
		// Nothing changed: put the marker back as it was — an older interrupted
		// marker survives, and one this call wrote is removed.
		if o.hadPrev {
			b, _ := json.Marshal(o.prev)
			_ = writeFileAtomic(filepath.Join(o.dir, pendingFile), b)
		} else if o.wrotePending {
			removePending(o.dir)
		}
	})
}
