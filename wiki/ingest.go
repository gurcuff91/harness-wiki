package wiki

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Ingest limits. They exist so a mistaken "/" or "~" cannot fill the disk; they are
// variables only so tests can lower them.
var (
	maxIngestFiles = 2000
	maxIngestBytes = int64(200 << 20) // 200 MB
)

// copyHook, when set by a test, runs before each file is copied; a non-nil error fails
// that copy. It lets a test prove that AddSources itself — not just its helper — undoes a
// copy that fails part way. nil in production.
var copyHook func(rel string) error

// source is one file to be copied into raw/: where it comes from and where it goes.
type source struct {
	rel     string // destination relative to raw/, "/"-separated
	from    string // absolute source path ("" for an inline file)
	content []byte // inline content (when from == "")
	size    int64
}

// AddSources implements Op. It copies every source into raw/ — all or nothing:
//
//  1. PLAN, without writing anything: resolve every source to a list of files and
//     destinations, validate each destination, and find the ones that already exist.
//     An existing file with identical content (same size and SHA-256) is "already
//     ingested" and left alone; one with DIFFERENT content is a conflict, and a single
//     conflict fails the whole ingest with the full list.
//  2. COPY each file to a temporary name and rename it into place, never overwriting.
//     If anything fails part way, what this call created is removed, so raw/ is exactly
//     as it was.
func (o *fsOp) AddSources(in Sources) (IngestResult, error) {
	if len(in.Paths) == 0 && len(in.Files) == 0 {
		return IngestResult{}, fmt.Errorf("%w: nothing to ingest — give at least one path (a file, a glob or a directory) or an inline file", ErrInvalidInput)
	}
	raw := filepath.Join(o.dir, "raw")
	if !isRealDir(raw) {
		return IngestResult{}, fmt.Errorf("wiki: %s is missing or is not a real folder", raw)
	}

	plan, skipped, err := o.plan(in)
	if err != nil {
		return IngestResult{}, err
	}

	var res IngestResult
	res.Skipped = skipped
	var toCopy []source
	var conflicts []string
	for _, src := range plan {
		dest, err := safeDest(raw, src.rel)
		if err != nil {
			return IngestResult{}, err
		}
		st, err := os.Lstat(dest)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			toCopy = append(toCopy, src)
		case err != nil:
			return IngestResult{}, fmt.Errorf("wiki: inspect %s: %w", src.rel, err)
		default:
			same, cerr := sameContent(dest, st, src)
			if cerr != nil {
				return IngestResult{}, cerr
			}
			if same {
				res.AlreadyIngested = append(res.AlreadyIngested, src.rel)
			} else {
				conflicts = append(conflicts, src.rel)
			}
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return IngestResult{}, &ConflictError{Paths: conflicts}
	}

	created, err := copyAll(raw, toCopy)
	if err != nil {
		removeCreated(created)
		return IngestResult{}, err
	}
	for _, src := range toCopy {
		res.Ingested = append(res.Ingested, src.rel)
	}
	sort.Strings(res.Ingested)
	sort.Strings(res.AlreadyIngested)
	return res, nil
}

// plan resolves in into the files to copy and the ones skipped, enforcing the limits
// and rejecting duplicate destinations — compared case-insensitively, because A.md and
// a.md are the same file on macOS and Windows.
func (o *fsOp) plan(in Sources) (plan []source, skipped []string, err error) {
	seen := map[string]string{} // lower-cased destination → the destination as first planned
	var total int64
	add := func(s source) error {
		key := strings.ToLower(s.rel)
		if prev, dup := seen[key]; dup {
			if prev == s.rel {
				return fmt.Errorf("%w: %q is named twice in this ingest", ErrInvalidInput, s.rel)
			}
			return fmt.Errorf("%w: %q and %q differ only in letter case — they are the same file on macOS and Windows", ErrInvalidInput, prev, s.rel)
		}
		seen[key] = s.rel
		total += s.size
		if len(plan)+1 > maxIngestFiles {
			return fmt.Errorf("%w: this ingest has more than %d files — ingest a smaller set (a mistaken '/' or '~'?)", ErrInvalidInput, maxIngestFiles)
		}
		if total > maxIngestBytes {
			return fmt.Errorf("%w: this ingest is larger than %d MB — ingest a smaller set (a mistaken '/' or '~'?)", ErrInvalidInput, maxIngestBytes>>20)
		}
		plan = append(plan, s)
		return nil
	}

	for _, p := range in.Paths {
		files, skips, perr := o.expandPath(p)
		if perr != nil {
			return nil, nil, perr
		}
		skipped = append(skipped, skips...)
		for _, f := range files {
			if err := add(f); err != nil {
				return nil, nil, err
			}
		}
	}
	for _, f := range in.Files {
		rel, verr := ValidRelPath(f.Name)
		if verr != nil {
			return nil, nil, verr
		}
		if err := add(source{rel: rel, content: []byte(f.Content), size: int64(len(f.Content))}); err != nil {
			return nil, nil, err
		}
	}
	return plan, skipped, nil
}

// expandPath resolves one entry of Sources.Paths: a file, a glob or a directory.
func (o *fsOp) expandPath(p string) (files []source, skipped []string, err error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return nil, nil, fmt.Errorf("%w: an empty source path", ErrInvalidInput)
	}
	// A path that exists is taken literally, even if it contains glob characters.
	if _, serr := os.Stat(p); serr == nil || !hasGlobMeta(p) {
		return o.expandLiteral(p)
	}
	return o.expandGlob(p)
}

// expandLiteral handles a path that names one file or one directory.
func (o *fsOp) expandLiteral(p string) ([]source, []string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, nil, fmt.Errorf("wiki: resolve %q: %w", p, err)
	}
	// A path the caller names explicitly is followed if it is a symlink: they asked
	// for it by name. (Symlinks found INSIDE a directory are another matter — below.)
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: source %q: %v", ErrInvalidInput, p, err)
	}
	if err := o.notInsideWiki(p, resolved); err != nil {
		return nil, nil, err
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: source %q: %v", ErrInvalidInput, p, err)
	}
	if !st.IsDir() {
		if !st.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("%w: source %q is not a regular file", ErrInvalidInput, p)
		}
		return []source{{rel: filepath.Base(abs), from: resolved, size: st.Size()}}, nil, nil
	}
	return o.walkDir(resolved, filepath.Base(abs))
}

// walkDir copies a directory whole, the directory itself included: "/x/ctx" lands as
// raw/ctx/…. Symlinks inside it are NOT followed — a link to ~/.ssh in a folder must
// not drag that into a wiki — they are skipped and reported.
func (o *fsOp) walkDir(root, base string) (files []source, skipped []string, err error) {
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		display := base + "/" + filepath.ToSlash(rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			skipped = append(skipped, display+": symlink")
			return nil // WalkDir does not follow symlinks; nothing to descend into
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			skipped = append(skipped, display+": not a regular file")
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		files = append(files, source{rel: display, from: path, size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("wiki: read directory %s: %w", root, err)
	}
	return files, skipped, nil
}

// expandGlob walks from the glob's literal prefix and keeps the regular files whose
// path matches. A glob selects files only; each lands in raw/ under its base name.
func (o *fsOp) expandGlob(pattern string) ([]source, []string, error) {
	absPattern, err := filepath.Abs(filepath.FromSlash(filepath.ToSlash(pattern)))
	if err != nil {
		return nil, nil, fmt.Errorf("wiki: resolve %q: %w", pattern, err)
	}
	slashPattern := filepath.ToSlash(absPattern)
	root, _ := globRoot(slashPattern)
	rootFS := filepath.FromSlash(root)
	if st, serr := os.Stat(rootFS); serr != nil || !st.IsDir() {
		return nil, nil, fmt.Errorf("%w: glob %q: the directory %q does not exist", ErrInvalidInput, pattern, root)
	}
	var files []source
	var skipped []string
	err = filepath.WalkDir(rootFS, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		slash := filepath.ToSlash(path)
		if !matchGlob(slashPattern, slash) {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			skipped = append(skipped, filepath.Base(path)+": symlink")
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if err := o.notInsideWiki(pattern, path); err != nil {
			return err
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		files = append(files, source{rel: filepath.Base(path), from: path, size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("wiki: glob %q: %w", pattern, err)
	}
	if len(files) == 0 && len(skipped) == 0 {
		return nil, nil, fmt.Errorf("%w: glob %q matches no files", ErrInvalidInput, pattern)
	}
	return files, skipped, nil
}

// notInsideWiki rejects a source that contains this wiki's folder or lies inside it:
// copying a folder into itself would never end, and ingesting the wiki's own pages as
// "raw sources" would defeat the point of raw/ being immutable.
func (o *fsOp) notInsideWiki(given, resolved string) error {
	wikiDir, err := filepath.EvalSymlinks(o.dir)
	if err != nil {
		wikiDir = o.dir
	}
	if within(resolved, wikiDir) || within(wikiDir, resolved) {
		return fmt.Errorf("%w: source %q overlaps the wiki's own folder %s — a wiki cannot ingest itself", ErrInvalidInput, given, wikiDir)
	}
	return nil
}

// within reports whether path is dir or lies inside it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// safeDest returns the absolute destination of a relative raw/ path, and proves it:
// the result must stay inside raw/, and no folder along the way may be a symlink (a
// wiki copied from another machine could carry one pointing anywhere).
func safeDest(raw, rel string) (string, error) {
	dest := filepath.Join(raw, filepath.FromSlash(rel))
	back, err := filepath.Rel(raw, dest)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) || filepath.IsAbs(back) {
		return "", fmt.Errorf("%w: %q leaves raw/", ErrInvalidPath, rel)
	}
	cur := raw
	for _, seg := range strings.Split(filepath.ToSlash(back), "/") {
		cur = filepath.Join(cur, seg)
		st, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			break // the rest does not exist yet, so it cannot be a link
		}
		if err != nil {
			return "", fmt.Errorf("wiki: inspect %s: %w", rel, err)
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: %q passes through a symbolic link inside raw/", ErrInvalidPath, rel)
		}
	}
	return dest, nil
}

// sameContent reports whether the existing file at dest has the same bytes as src.
func sameContent(dest string, st os.FileInfo, src source) (bool, error) {
	if !st.Mode().IsRegular() || st.Size() != src.size {
		return false, nil
	}
	a, err := hashFile(dest)
	if err != nil {
		return false, fmt.Errorf("wiki: read %s: %w", dest, err)
	}
	var b [sha256.Size]byte
	if src.from == "" {
		b = sha256.Sum256(src.content)
	} else if b, err = hashFile(src.from); err != nil {
		return false, fmt.Errorf("wiki: read %s: %w", src.from, err)
	}
	return a == b, nil
}

func hashFile(path string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// created records what copyAll made, so a failure can undo it: files first, then the
// folders it had to create, deepest first.
type created struct {
	files []string
	dirs  []string
}

// copyAll copies every source into raw/ and returns what it created. A file is written
// under a temporary name and renamed into place, so a reader never sees a partial file;
// an existing destination is never overwritten (the plan already excluded those, but a
// concurrent writer could have created one since — os.Link fails instead of replacing).
func copyAll(raw string, srcs []source) (created, error) {
	var c created
	for _, src := range srcs {
		dest := filepath.Join(raw, filepath.FromSlash(src.rel))
		if err := mkdirAllTracked(raw, filepath.Dir(dest), &c); err != nil {
			return c, err
		}
		if copyHook != nil {
			if err := copyHook(src.rel); err != nil {
				return c, err
			}
		}
		if err := copyOne(dest, src); err != nil {
			return c, err
		}
		c.files = append(c.files, dest)
	}
	return c, nil
}

// copyOne writes src to dest through a temp file in the same folder, and links it into
// place so an existing dest is an error rather than being replaced.
func copyOne(dest string, src source) (err error) {
	tmp, terr := os.CreateTemp(filepath.Dir(dest), ".ingest-*")
	if terr != nil {
		return fmt.Errorf("wiki: write %s: %w", src.rel, terr)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		_ = os.Remove(tmpName) // gone already after a successful link; harmless either way
	}()
	if src.from == "" {
		_, err = io.Copy(tmp, bytes.NewReader(src.content))
	} else {
		var in *os.File
		if in, err = os.Open(src.from); err != nil {
			return fmt.Errorf("wiki: read %s: %w", src.from, err)
		}
		defer in.Close()
		_, err = io.Copy(tmp, in)
	}
	if err != nil {
		return fmt.Errorf("wiki: write %s: %w", src.rel, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("wiki: write %s: %w", src.rel, err)
	}
	// Sources are read-only in spirit; keep the file readable by everyone who can read
	// the wiki, never executable.
	_ = os.Chmod(tmpName, 0o644)
	if err := os.Link(tmpName, dest); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("wiki: %s appeared while ingesting — nothing was overwritten", src.rel)
		}
		// A filesystem without hard links: fall back to a rename guarded by an
		// existence check (the race window is tiny and the plan already checked).
		if _, serr := os.Lstat(dest); serr == nil {
			return fmt.Errorf("wiki: %s appeared while ingesting — nothing was overwritten", src.rel)
		}
		if rerr := os.Rename(tmpName, dest); rerr != nil {
			return fmt.Errorf("wiki: write %s: %w", src.rel, rerr)
		}
	}
	return nil
}

// mkdirAllTracked creates dir and any missing parents under raw, recording the ones it
// created (deepest last) so a rollback removes exactly those.
func mkdirAllTracked(raw, dir string, c *created) error {
	var missing []string
	for d := dir; d != raw && d != filepath.Dir(d); d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil {
			break
		}
		missing = append(missing, d)
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("wiki: create folder %s: %w", missing[i], err)
		}
		c.dirs = append(c.dirs, missing[i])
	}
	return nil
}

// removeCreated undoes a failed copy: the files first, then the folders it created,
// deepest first, and only if they are empty (never something that was not ours).
func removeCreated(c created) {
	for _, f := range c.files {
		_ = os.Remove(f)
	}
	for i := len(c.dirs) - 1; i >= 0; i-- {
		_ = os.Remove(c.dirs[i])
	}
}
