package wiki

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fastLock shrinks the lock timings so a test can watch a heartbeat and an expiry in
// milliseconds, and restores them afterwards.
func fastLock(t *testing.T) {
	t.Helper()
	oldBeat, oldStale := heartbeatEvery, staleAfter
	heartbeatEvery, staleAfter = 20*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { heartbeatEvery, staleAfter = oldBeat, oldStale })
}

func TestLockIsExclusiveAndFailsAtOnce(t *testing.T) {
	dir := t.TempDir()
	l1, err := acquireLock("w", dir, "ingest")
	if err != nil {
		t.Fatal(err)
	}
	defer l1.release()

	start := time.Now()
	_, err = acquireLock("w", dir, "query")
	if time.Since(start) > time.Second {
		t.Errorf("a busy wiki took %v to refuse: it must fail at once, never wait", time.Since(start))
	}
	var busy *ErrBusy
	if !errors.As(err, &busy) {
		t.Fatalf("err = %v, want *ErrBusy", err)
	}
	if busy.Op != "ingest" || busy.Slug != "w" || busy.Since.IsZero() {
		t.Errorf("ErrBusy = %+v: it must say what is running and since when", busy)
	}
	if !IsBusy(err) {
		t.Error("IsBusy must recognise it")
	}
	// the Wiki* tools recognise it through this one-method interface, without importing wiki
	var marker interface{ WikiBusy() bool }
	if !errors.As(err, &marker) || !marker.WikiBusy() {
		t.Error("*ErrBusy must implement WikiBusy() bool")
	}
	if msg := busy.Error(); !strings.Contains(msg, "busy") || !strings.Contains(msg, "ingest") || !strings.Contains(msg, "try again later") {
		t.Errorf("message = %q: it must say busy, what is running, and to try again later", msg)
	}
}

func TestLockCanBeTakenAgainAfterRelease(t *testing.T) {
	dir := t.TempDir()
	l, err := acquireLock("w", dir, "a")
	if err != nil {
		t.Fatal(err)
	}
	l.release()
	l.release() // idempotent
	if _, err := os.Stat(filepath.Join(dir, lockFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the lock file survived release: %v", err)
	}
	l2, err := acquireLock("w", dir, "b")
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	l2.release()
}

// The heartbeat is what keeps a LONG operation alive: with a short staleAfter, a lock
// that is renewed must stay live well past it.
func TestHeartbeatKeepsALongOperationAlive(t *testing.T) {
	fastLock(t)
	dir := t.TempDir()
	l, err := acquireLock("w", dir, "ingest")
	if err != nil {
		t.Fatal(err)
	}
	defer l.release()

	time.Sleep(4 * staleAfter) // four times the age at which an un-renewed lock is dead
	if _, live := lockState(dir); !live {
		t.Fatal("a renewed lock was treated as abandoned: a long ingest would have its lock stolen")
	}
	if _, err := acquireLock("w", dir, "other"); !IsBusy(err) {
		t.Fatalf("a live lock was taken over: %v", err)
	}
	if l.ctx.Err() != nil {
		t.Errorf("the owner's context was cancelled while it still held the lock: %v", l.ctx.Err())
	}
}

// A dead owner (no heartbeat) loses the lock after staleAfter.
func TestAbandonedLockIsReclaimed(t *testing.T) {
	fastLock(t)
	dir := t.TempDir()
	// A lock file nobody renews: written directly, as a crashed process leaves it.
	if err := createExclusive(filepath.Join(dir, lockFile), lockInfo{Op: "ingest", StartedAt: time.Now().UTC(), Token: "dead"}); err != nil {
		t.Fatal(err)
	}
	if _, live := lockState(dir); !live {
		t.Fatal("a fresh lock must be live")
	}
	if _, err := acquireLock("w", dir, "query"); !IsBusy(err) {
		t.Fatalf("a fresh lock must refuse: %v", err)
	}
	time.Sleep(2 * staleAfter)
	if _, live := lockState(dir); live {
		t.Fatal("a lock nobody renewed is still live")
	}
	l, err := acquireLock("w", dir, "query")
	if err != nil {
		t.Fatalf("an abandoned lock must be reclaimed: %v", err)
	}
	defer l.release()
	cur, _, _ := readLockFile(filepath.Join(dir, lockFile))
	if cur.Op != "query" || cur.Token != l.token {
		t.Errorf("the lock file now holds %+v, want the new owner's", cur)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != lockFile {
			t.Errorf("reclaiming left %q behind", e.Name())
		}
	}
}

// Many processes racing to reclaim one dead lock: exactly one wins.
func TestReclaimingAnAbandonedLockHasOneWinner(t *testing.T) {
	fastLock(t)
	for round := 0; round < 20; round++ {
		dir := t.TempDir()
		if err := createExclusive(filepath.Join(dir, lockFile), lockInfo{Op: "x", StartedAt: time.Now().UTC(), Token: "dead"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(staleAfter + 50*time.Millisecond)

		const racers = 12
		var wins atomic.Int32
		var locks sync.Map
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				l, err := acquireLock("w", dir, "race")
				if err == nil {
					wins.Add(1)
					locks.Store(l, true)
				} else if !IsBusy(err) {
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if n := wins.Load(); n != 1 {
			t.Fatalf("round %d: %d goroutines own the lock, want exactly 1", round, n)
		}
		locks.Range(func(k, _ any) bool { k.(*heldLock).release(); return true })
	}
}

// A process suspended longer than staleAfter (Ctrl+Z) wakes up to find another owns
// the wiki: its context is cancelled and — crucially — it must NOT delete the new
// owner's lock when it finally releases.
func TestLostLockCancelsTheOwnerAndNeverDeletesTheSuccessors(t *testing.T) {
	fastLock(t)
	dir := t.TempDir()
	l, err := acquireLock("w", dir, "ingest")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the suspension: someone else replaces the lock file with their own.
	path := filepath.Join(dir, lockFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := createExclusive(path, lockInfo{Op: "usurper", StartedAt: time.Now().UTC(), Token: "someone-else"}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-l.ctx.Done():
		if !errors.Is(context.Cause(l.ctx), errLockLost) {
			t.Errorf("cause = %v, want errLockLost", context.Cause(l.ctx))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the heartbeat never noticed the lock was taken over")
	}
	l.release()
	cur, _, ok := readLockFile(path)
	if !ok || cur.Token != "someone-else" {
		t.Fatalf("release deleted the successor's lock (ok=%v, %+v)", ok, cur)
	}
}

func TestReleaseWithTheWrongTokenIsANoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, lockFile)
	if err := createExclusive(path, lockInfo{Op: "x", StartedAt: time.Now().UTC(), Token: "real"}); err != nil {
		t.Fatal(err)
	}
	impostor := &heldLock{path: path, token: "impostor", stop: make(chan struct{}), done: make(chan struct{})}
	impostor.ctx, impostor.cancel = context.WithCancelCause(context.Background())
	close(impostor.done) // no heartbeat goroutine in this hand-made lock
	impostor.release()
	if _, _, ok := readLockFile(path); !ok {
		t.Fatal("a release with the wrong token removed someone else's lock")
	}
}

// The lock file must never be left open: Windows cannot delete or replace a file
// another handle holds. Proof by behaviour — it can be removed and renamed while the
// lock is held (which would fail on Windows with an open handle).
func TestLockFileIsNotHeldOpen(t *testing.T) {
	dir := t.TempDir()
	l, err := acquireLock("w", dir, "ingest")
	if err != nil {
		t.Fatal(err)
	}
	defer l.release()
	path := filepath.Join(dir, lockFile)
	if err := os.Rename(path, path+".moved"); err != nil {
		t.Fatalf("the lock file cannot be renamed while held — a handle is open: %v", err)
	}
	if err := os.Rename(path+".moved", path); err != nil {
		t.Fatal(err)
	}
}

// A torn / foreign lock file still counts as a lock (it exists), with no details.
func TestUnreadableLockFileStillCountsAsALockUntilItGoesStale(t *testing.T) {
	fastLock(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, lockFile), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, live := lockState(dir); !live {
		t.Error("a fresh unreadable lock file must still be live")
	}
	if _, err := acquireLock("w", dir, "q"); !IsBusy(err) {
		t.Errorf("err = %v, want busy", err)
	}
	time.Sleep(2 * staleAfter)
	l, err := acquireLock("w", dir, "q")
	if err != nil {
		t.Fatalf("a stale unreadable lock must be reclaimed: %v", err)
	}
	l.release()
}

func TestPendingMarkerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok := readPending(dir); ok {
		t.Fatal("no marker yet")
	}
	if err := writePending(dir, "ingest"); err != nil {
		t.Fatal(err)
	}
	p, ok := readPending(dir)
	if !ok || p.Op != "ingest" || p.StartedAt.IsZero() {
		t.Fatalf("pending = %+v ok=%v", p, ok)
	}
	removePending(dir)
	if _, ok := readPending(dir); ok {
		t.Fatal("marker survived removal")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("writing the marker left temp files: %v", entries)
	}
}
