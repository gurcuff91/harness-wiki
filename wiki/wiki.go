// Package wiki is the persistence layer for LLM-maintained wikis, after the "LLM
// Wiki" pattern: instead of re-deriving knowledge from raw documents on every
// question, a model incrementally builds and maintains a persistent, interlinked
// collection of markdown pages between the user and the sources.
//
// A Store manages SEVERAL independent wikis. Each wiki is a folder named by its
// slug:
//
//	<root>/<slug>/
//	  wiki.json     manifest {slug, description, created_at} — the only source of truth
//	  AGENTS.md     the wiki's schema; the maintainer reads it because the folder is its cwd
//	  raw/          immutable sources
//	  wiki/         pages (index.md and log.md are maintained by the maintainer)
//	  .lock         present only while an operation runs
//	  .pending      present only if an ingest or lint did not finish cleanly
//
// The store is pure persistence: it never invokes a model. The AI work — adapting
// the schema, ingesting, answering, linting — is done by the Wiki* tools, which run
// a sub-agent with the wiki folder as its working directory and use the Store only
// to create folders, copy sources and take the per-wiki lock.
//
// This package is part of the SDK: it imports nothing from agent/, server or
// internal/providers, and an SDK consumer can implement Store for another backend
// or open the default filesystem one with OpenFS.
package wiki

import (
	"context"
	"time"
)

// Status is the live state of a wiki. It is never stored: List computes it from
// the wiki's lock and pending files every time, so it cannot go stale.
type Status string

const (
	// StatusReady: no operation is running and none was left half done.
	StatusReady Status = "ready"
	// StatusBusy: an operation is running right now (a lock with a fresh heartbeat).
	StatusBusy Status = "busy"
	// StatusIncomplete: an ingest or lint was interrupted (a pending marker with no
	// live lock). A lint is the way to check and repair it.
	StatusIncomplete Status = "incomplete"
)

// Info describes one wiki.
type Info struct {
	Slug        string    `json:"slug"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	Status      Status    `json:"status"`
	// Op and Since say what is running (busy) or what was interrupted (incomplete),
	// and since when. Empty for a ready wiki.
	Op    string    `json:"op,omitempty"`
	Since time.Time `json:"since,omitzero"`
}

// File is an inline source: written to raw/<Name>. Name may contain "/" to create
// subfolders ("ctx/data.md" → raw/ctx/data.md).
type File struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// Sources is what an ingest adds to raw/.
type Sources struct {
	// Paths are files, globs (with ** across directories) or directories on the
	// local filesystem. A directory is copied whole, directory included.
	Paths []string
	// Files are inline sources written as files.
	Files []File
}

// IngestResult lists what an ingest did, with paths relative to raw/ and "/" as
// the separator.
type IngestResult struct {
	// Ingested are the files copied now.
	Ingested []string `json:"ingested"`
	// AlreadyIngested had identical content in raw/ already and were left alone.
	AlreadyIngested []string `json:"already_ingested,omitempty"`
	// Skipped are source files that were not copied, each with the reason
	// (for example a symbolic link found inside a directory).
	Skipped []string `json:"skipped,omitempty"`
}

// Op is one in-flight operation on a wiki: it holds the wiki's lock until Done or
// Abort. Exactly one of them must be called, once.
type Op interface {
	// Slug is the wiki this operation works on.
	Slug() string
	// Dir is the wiki's folder — the working directory for its maintainer.
	Dir() string
	// Interrupted is the earlier operation that did not finish cleanly, if any (the
	// wiki was left with a pending marker). The maintainer is told to verify the
	// wiki's coherence first.
	Interrupted() (op string, since time.Time, ok bool)
	// Context is cancelled if the lock is lost (this process was suspended long
	// enough for another to take over the wiki). Work derived from it stops.
	Context() context.Context
	// AddSources copies sources into raw/ — all or nothing. Only valid while the
	// operation is held.
	AddSources(Sources) (IngestResult, error)
	// Done ends the operation. ok=true removes any pending marker; ok=false leaves it
	// (an ingest or lint that failed or was cancelled), so the next operation and
	// List know the wiki may be half updated. The lock is released either way.
	Done(ok bool)
	// Abort ends an operation in which nothing was changed (a validation error, a
	// clash in raw/): it releases the lock and removes the pending marker it wrote,
	// leaving the wiki exactly as it was, including any older interrupted marker.
	Abort()
}

// View is a read-only handle on a wiki, for work that takes NO lock: a query. Any number can
// exist at once, alongside each other and alongside an ingest, a lint or a delete — a query may
// write pages of its own (it files valuable answers back into the wiki), but those are related to
// the question asked, so nothing serialises them. What a View does NOT give is any protection:
// it sees a wiki that may be mid-ingest, and the wiki may be deleted under it.
type View interface {
	// Slug is the wiki this view is on.
	Slug() string
	// Dir is the wiki's folder — the working directory for its maintainer.
	Dir() string
	// Interrupted is an earlier ingest or lint that did NOT finish cleanly: a pending marker
	// with no live lock. A marker WITH a live lock is an operation running right now, not an
	// interrupted one, and is not reported.
	Interrupted() (op string, since time.Time, ok bool)
}

// Store manages a collection of wikis.
type Store interface {
	// Init creates a wiki: the folders, the base AGENTS.md, the seed index and log,
	// and — last — the manifest. It never overwrites: an existing wiki is ErrExists.
	// If it fails part way it removes what it created.
	Init(slug, description string) (Info, error)
	// List returns every wiki, sorted by slug. A folder without a readable manifest
	// is not a wiki and is ignored.
	List() ([]Info, error)
	// Get returns one wiki, or ErrNotFound.
	Get(slug string) (Info, error)
	// Delete removes a wiki entirely. It takes the lock, so a busy wiki is *ErrBusy.
	Delete(slug string) error
	// Begin takes the wiki's lock for op and returns at once with *ErrBusy if another
	// operation holds it (it never waits). Operations "ingest" and "lint" also write
	// the pending marker; "init" does not. A query does not use Begin at all: see View.
	Begin(slug, op string) (Op, error)
	// View opens a wiki for reading WITHOUT taking its lock, so queries run concurrently with
	// each other and with every other operation. It fails with ErrNotFound (or ErrInvalidSlug)
	// exactly as Begin does, but never with *ErrBusy.
	View(slug string) (View, error)
	// Close releases the store's resources. It does not touch the wikis on disk.
	Close() error
}
