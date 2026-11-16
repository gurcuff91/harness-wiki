// Package service is the layer between the HTTP API and the two packages below it: it runs each
// wiki operation — create, delete, ingest, lint, query — by combining the store (persistence, the
// lock, the pending marker) with the maintainer (the model). It holds the rules of an operation:
// which ones take the lock, which run in the background, when the pending marker is kept, what is
// reported when something is cut short. The API above it only translates HTTP.
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gurcuff91/harness-wiki/maintainer"
	"github.com/gurcuff91/harness-wiki/wiki"
)

// Defaults and caps for the synchronous operations, in seconds on the wire.
const (
	// InitTimeout bounds the schema adaptation of a new wiki.
	InitTimeout = 120 * time.Second
	// QueryTimeout is a query's default; MaxTimeout caps whatever a caller asks for.
	QueryTimeout = 300 * time.Second
	MaxTimeout   = 30 * time.Minute
)

// Maintainer is what the service needs from the AI side. *maintainer.Runner implements it; tests
// substitute a fake, so the operation rules can be checked without a model.
type Maintainer interface {
	Execute(ctx context.Context, run maintainer.Run) (string, error)
}

// Service runs wiki operations.
type Service struct {
	Store wiki.Store
	AI    Maintainer

	mu      sync.Mutex
	closed  bool
	running sync.WaitGroup // background ingests and lints still in flight
}

// New returns a Service over a store and a maintainer.
func New(store wiki.Store, ai Maintainer) *Service {
	return &Service{Store: store, AI: ai}
}

// Close stops accepting background work and waits up to grace for what is running to finish. The
// caller cancels the maintainers themselves (Runner.Shutdown); this only waits for their
// goroutines to end, so a clean shutdown leaves no wiki locked.
func (s *Service) Close(grace time.Duration) {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.running.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
	}
}

// background registers a goroutine; ok is false once the service is closing.
func (s *Service) background() (end func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false
	}
	s.running.Add(1)
	return s.running.Done, true
}

// ErrInvalidInput is returned for a request that is wrong in itself: a missing or blank field. It
// is the caller's to fix. (The store has its own for what it can see: wiki.ErrInvalidInput.)
var ErrInvalidInput = wiki.ErrInvalidInput

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{wiki.ErrInvalidInput}, a...)...)
}

// List returns every wiki with its live status. The slice is never nil.
func (s *Service) List() ([]wiki.Info, error) {
	list, err := s.Store.List()
	if list == nil {
		list = []wiki.Info{}
	}
	return list, err
}

// Created is the result of Create.
type Created struct {
	Info wiki.Info
	// Adapted is true when the maintainer adapted the wiki's schema to its description. When it
	// is false the wiki still exists and is usable with the base schema; AdaptError says why.
	Adapted      bool
	AdaptedError string
}

// Create makes a wiki, then has the maintainer adapt its AGENTS.md to the description. The store
// part is deterministic; the adaptation is the AI part and may fail without undoing the wiki.
func (s *Service) Create(ctx context.Context, slug, description, model string) (Created, error) {
	if strings.TrimSpace(description) == "" {
		return Created{}, invalid("a description is required")
	}
	info, err := s.Store.Init(slug, description)
	if err != nil {
		return Created{}, err
	}
	out := Created{Info: info}
	adapted, reason := s.adapt(ctx, info, model)
	// info is what Init returned: the wiki as created, ready. The adaptation held the lock only
	// for a moment and has released it, so there is nothing to refresh.
	out.Adapted, out.AdaptedError = adapted, reason
	return out, nil
}

// adapt runs the maintainer that adapts a new wiki's schema to its description. It reports whether
// it did and, when it did not, why. The lock is released before it returns.
func (s *Service) adapt(ctx context.Context, info wiki.Info, model string) (adapted bool, reason string) {
	op, err := s.Store.Begin(info.Slug, "init")
	if err != nil {
		return false, err.Error() // created; the adaptation could not even start
	}
	ok := false
	defer func() { op.Done(ok) }()
	rctx, cancel := context.WithTimeout(merge(ctx, op.Context()), InitTimeout)
	defer cancel()
	if _, err := s.AI.Execute(rctx, maintainer.Run{
		Slug: info.Slug, Dir: op.Dir(), Op: maintainer.OpInit, Model: model,
		Prompt: Prompt(maintainer.OpInit, PromptArgs{Subject: info.Description}),
	}); err != nil {
		return false, err.Error()
	}
	ok = true
	return true, ""
}

// Delete removes a wiki. A wiki that an ingest, a lint or another delete holds is *wiki.ErrBusy.
func (s *Service) Delete(slug string) error { return s.Store.Delete(slug) }

// Accepted is the result of Ingest and Lint: the job was started and keeps running in the
// background. There is no result report; the wiki's status (busy → ready, or incomplete if it was
// cut short) is how a caller follows it.
type Accepted struct {
	Slug   string
	Ingest wiki.IngestResult // ingest only
	// Interrupted reports that an earlier ingest or lint had been cut short; the maintainer was
	// asked to verify the wiki's coherence first.
	Interrupted bool
}

// Ingest copies the sources into the wiki's raw/ folder and starts its maintainer in the
// background. It returns as soon as the sources are copied. A clash, a bad name or a bad source
// refuses the whole ingest and leaves the wiki exactly as it was.
func (s *Service) Ingest(slug string, src wiki.Sources, guidance, model string) (Accepted, error) {
	if len(src.Paths) == 0 && len(src.Files) == 0 {
		return Accepted{}, invalid("nothing to ingest — give 'sources' (paths, globs or directories) and/or 'files' (inline name + content)")
	}
	end, ok := s.background()
	if !ok {
		return Accepted{}, errors.New("the server is shutting down")
	}
	op, err := s.Store.Begin(slug, "ingest")
	if err != nil {
		end()
		return Accepted{}, err
	}
	res, err := op.AddSources(src)
	if err != nil {
		op.Abort() // nothing changed: the wiki is as it was
		end()
		return Accepted{}, err
	}
	prevOp, prevSince, interrupted := op.Interrupted()
	prompt := Prompt(maintainer.OpIngest, PromptArgs{
		Guidance: guidance, Ingested: res.Ingested, AlreadyIngested: res.AlreadyIngested,
		Interrupted: interrupted, PrevOp: prevOp, PrevSince: prevSince,
	})
	s.runBackground(op, end, maintainer.Run{Slug: slug, Dir: op.Dir(), Op: maintainer.OpIngest, Prompt: prompt, Model: model})
	return Accepted{Slug: slug, Ingest: res, Interrupted: interrupted}, nil
}

// Lint health-checks a wiki and repairs it, in the background.
func (s *Service) Lint(slug, guidance, model string) (Accepted, error) {
	end, ok := s.background()
	if !ok {
		return Accepted{}, errors.New("the server is shutting down")
	}
	op, err := s.Store.Begin(slug, "lint")
	if err != nil {
		end()
		return Accepted{}, err
	}
	prevOp, prevSince, interrupted := op.Interrupted()
	prompt := Prompt(maintainer.OpLint, PromptArgs{Guidance: guidance, Interrupted: interrupted, PrevOp: prevOp, PrevSince: prevSince})
	s.runBackground(op, end, maintainer.Run{Slug: slug, Dir: op.Dir(), Op: maintainer.OpLint, Prompt: prompt, Model: model})
	return Accepted{Slug: slug, Interrupted: interrupted}, nil
}

// runBackground runs the maintainer for an already-begun operation in a goroutine that owns the
// operation from here: it recovers a panic, and it ends the operation — Done(true) only when the
// maintainer finished cleanly, so a failure, a cancellation or a panic leaves the pending marker.
//
// It deliberately ignores any request context: the work must outlive the HTTP request that
// started it. It stops when the operation's own context ends (the wiki's lock was lost) or when
// the maintainer runner is shut down.
func (s *Service) runBackground(op wiki.Op, end func(), run maintainer.Run) {
	go func() {
		ok := false
		defer func() {
			if r := recover(); r != nil {
				ok = false
			}
			op.Done(ok)
			end()
		}()
		if _, err := s.AI.Execute(op.Context(), run); err == nil {
			ok = true
		}
	}()
}

// Query asks a wiki a question and waits for the answer. It takes NO lock: any number can run at
// once, even while an ingest or a lint works on the wiki (the maintainer may then see pages that
// are still being updated). A query may write pages of its own, but they relate to its question.
func (s *Service) Query(ctx context.Context, slug, question string, timeout time.Duration, model string) (string, error) {
	if strings.TrimSpace(question) == "" {
		return "", invalid("a question is required")
	}
	view, err := s.Store.View(slug)
	if err != nil {
		return "", err
	}
	timeout = ClampTimeout(timeout, QueryTimeout)
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	prevOp, prevSince, interrupted := view.Interrupted()
	out, err := s.AI.Execute(rctx, maintainer.Run{
		Slug: slug, Dir: view.Dir(), Op: maintainer.OpQuery, Model: model,
		Prompt: Prompt(maintainer.OpQuery, PromptArgs{Question: question, Interrupted: interrupted, PrevOp: prevOp, PrevSince: prevSince}),
	})
	if err != nil && errors.Is(rctx.Err(), context.DeadlineExceeded) {
		return "", &TimeoutError{After: timeout}
	}
	return out, err
}

// TimeoutError is returned by Query when the maintainer did not answer in time. The partial
// answer is discarded: a model cut off mid-thought would only mislead.
type TimeoutError struct{ After time.Duration }

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("timeout after %v: the maintainer did not answer in time — retry with a larger timeout", e.After)
}

// ClampTimeout returns d, the default when d is not positive, and never more than MaxTimeout.
func ClampTimeout(d, def time.Duration) time.Duration {
	if d <= 0 {
		d = def
	}
	if d > MaxTimeout {
		d = MaxTimeout
	}
	return d
}

// merge returns a context that ends when either a or b does. a's values win. It leaks nothing:
// the registration on b is dropped as soon as the merged context ends.
func merge(a, b context.Context) context.Context {
	ctx, cancel := context.WithCancelCause(a)
	stop := context.AfterFunc(b, func() { cancel(context.Cause(b)) })
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx
}
