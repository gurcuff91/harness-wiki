package wiki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// The per-wiki lock. It is deliberately NOT internal/filelock: that primitive is
// built for read-modify-write cycles of a few milliseconds and treats a lock older
// than 45 s as abandoned — an ingest runs for minutes, so a live operation would
// have its lock stolen. This one is kept alive by a heartbeat instead: the owner
// renews the lock file's modification time every heartbeatEvery, and a lock that
// has not been renewed for staleAfter belongs to a dead owner.
//
// It uses one portable primitive — O_CREATE|O_EXCL is atomic on every OS Go
// supports — and no PIDs, so there is nothing platform-specific. Windows cannot
// delete or replace a file another handle holds open, so the lock file is never
// kept open: it is created, written and closed at once, and afterwards only its
// timestamp is touched.

// Both are variables so tests can shrink them; production code never changes them.
var (
	heartbeatEvery = 5 * time.Second
	staleAfter     = 30 * time.Second
)

const (
	lockFile    = ".lock"
	pendingFile = ".pending"
	// releaseAttempts/releaseDelay: removing the lock can fail for a moment on
	// Windows (an antivirus or an indexer holding the file). If it never works the
	// heartbeat has stopped, so the lock simply expires.
	releaseAttempts = 5
	releaseDelay    = 20 * time.Millisecond
)

// lockInfo is the content of .lock and of .pending.
type lockInfo struct {
	Op        string    `json:"op"`
	StartedAt time.Time `json:"started_at"`
	Token     string    `json:"token,omitempty"`
}

// heldLock is a lock this process owns.
type heldLock struct {
	path  string
	token string
	info  lockInfo

	cancel context.CancelCauseFunc
	ctx    context.Context

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// readLock reads a lock or pending file. ok is false when it does not exist or is
// unreadable (an unreadable lock is treated like a missing one only by callers
// that also check the age — see lockState).
func readLockFile(path string) (info lockInfo, mtime time.Time, ok bool) {
	st, err := os.Stat(path)
	if err != nil {
		return lockInfo{}, time.Time{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return lockInfo{}, time.Time{}, false
	}
	if err := json.Unmarshal(b, &info); err != nil {
		// A torn or foreign file: still a lock (it exists), with unknown details.
		return lockInfo{}, st.ModTime(), true
	}
	return info, st.ModTime(), true
}

// lockState reports whether a LIVE lock exists on dir: its info when so. A lock
// whose heartbeat is older than staleAfter is dead and reported as no lock.
func lockState(dir string) (info lockInfo, live bool) {
	info, mtime, ok := readLockFile(filepath.Join(dir, lockFile))
	if !ok {
		return lockInfo{}, false
	}
	if time.Since(mtime) > staleAfter {
		return info, false
	}
	return info, true
}

// acquireLock takes dir's lock for op, or returns *ErrBusy at once if a live
// operation holds it — it never waits. The returned lock's context is cancelled
// if the lock is lost (see heartbeat).
func acquireLock(slug, dir, op string) (*heldLock, error) {
	path := filepath.Join(dir, lockFile)
	for attempt := 0; attempt < 5; attempt++ {
		token := uuid.NewString()
		info := lockInfo{Op: op, StartedAt: time.Now().UTC(), Token: token}
		err := createExclusive(path, info)
		if err == nil {
			l := &heldLock{path: path, token: token, info: info, stop: make(chan struct{}), done: make(chan struct{})}
			l.ctx, l.cancel = context.WithCancelCause(context.Background())
			go l.heartbeat()
			return l, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("wiki lock: %w", err)
		}
		// Someone holds it. A live holder means busy; a dead one is reclaimed.
		held, mtime, ok := readLockFile(path)
		if !ok {
			continue // vanished between create and read: try again
		}
		if time.Since(mtime) <= staleAfter {
			return nil, &ErrBusy{Slug: slug, Op: held.Op, Since: held.StartedAt}
		}
		reclaimStale(path, held, mtime)
	}
	// Lost the race for a reclaimed lock several times: someone else is active.
	held, _, _ := readLockFile(path)
	return nil, &ErrBusy{Slug: slug, Op: held.Op, Since: held.StartedAt}
}

// createExclusive creates path (failing with os.ErrExist if it exists), writes the
// lock info and closes the file immediately.
func createExclusive(path string, info lockInfo) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(info)
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(path)
		return errors.Join(werr, cerr)
	}
	return nil
}

// reclaimStale removes a dead lock — but ONLY the one the caller judged dead.
//
// "Is it stale?" and "remove it" are two steps, and between them another process
// may have reclaimed the lock and created a fresh LIVE one at the same path.
// Removing or renaming by path would then destroy that live lock — and putting it
// back afterwards is not atomic, so a third process can slip in. Re-checking after
// the fact cannot close that gap; only making the whole reclaim exclusive can.
//
// So reclaiming is serialised by a second file, .lock.reclaim, created with
// O_EXCL: whoever creates it is the only process allowed to remove a lock. Under
// it the lock is read AGAIN, and it is removed only if it is still the very one that
// was judged dead (same token, same age). Everyone else just gives up the attempt
// and lets acquireLock re-read the new state.
func reclaimStale(path string, judged lockInfo, judgedMtime time.Time) {
	claim := path + ".reclaim"
	f, err := os.OpenFile(claim, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		// Another process is reclaiming right now — unless its claim is itself
		// abandoned (it died mid-reclaim): that file is stale like any lock.
		if st, serr := os.Stat(claim); serr == nil && time.Since(st.ModTime()) > staleAfter {
			_ = os.Remove(claim)
		}
		return
	}
	_ = f.Close()
	defer os.Remove(claim)

	cur, mtime, ok := readLockFile(path)
	if !ok {
		return // already gone
	}
	if cur.Token != judged.Token || !mtime.Equal(judgedMtime) || time.Since(mtime) <= staleAfter {
		return // not the lock we judged: somebody replaced it — it is theirs now
	}
	_ = os.Remove(path)
}

// heartbeat renews the lock's mtime until stopped, and checks the token on every
// beat: if the file is gone or belongs to someone else (this process was suspended
// longer than staleAfter and another reclaimed the lock), the operation no longer
// owns the wiki and its context is cancelled.
func (l *heldLock) heartbeat() {
	defer close(l.done)
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			cur, _, ok := readLockFile(l.path)
			if !ok || cur.Token != l.token {
				l.cancel(errLockLost)
				return
			}
			now := time.Now()
			if err := os.Chtimes(l.path, now, now); err != nil {
				l.cancel(errLockLost)
				return
			}
		}
	}
}

var errLockLost = errors.New("wiki lock lost: another process took over the wiki")

// release stops the heartbeat and removes the lock file — but only if it still
// carries this lock's token, so a process that lost its lock never deletes its
// successor's.
func (l *heldLock) release() {
	l.stopOnce.Do(func() {
		close(l.stop)
		<-l.done
		for i := 0; i < releaseAttempts; i++ {
			cur, _, ok := readLockFile(l.path)
			if !ok || cur.Token != l.token {
				break
			}
			if err := os.Remove(l.path); err == nil || errors.Is(err, os.ErrNotExist) {
				break
			}
			time.Sleep(releaseDelay)
		}
		l.cancel(nil)
	})
}

// writePending records that an operation started and has not finished cleanly.
func writePending(dir, op string) error {
	b, _ := json.Marshal(lockInfo{Op: op, StartedAt: time.Now().UTC()})
	tmp := filepath.Join(dir, pendingFile+".tmp-"+uuid.NewString())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, pendingFile)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func removePending(dir string) { _ = os.Remove(filepath.Join(dir, pendingFile)) }

// readPending returns the pending marker, if one exists.
func readPending(dir string) (lockInfo, bool) {
	info, _, ok := readLockFile(filepath.Join(dir, pendingFile))
	return info, ok
}
