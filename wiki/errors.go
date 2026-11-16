package wiki

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned when no wiki has the given slug (the folder is missing,
// or it has no readable manifest).
var ErrNotFound = errors.New("wiki not found")

// ErrExists is returned by Init when the wiki already exists.
var ErrExists = errors.New("wiki already exists")

// ErrInvalidSlug is returned for a slug that cannot name a wiki folder portably.
// It is wrapped with the reason; match it with errors.Is.
var ErrInvalidSlug = errors.New("invalid wiki slug")

// ErrInvalidPath is returned for a relative path (an inline file's name) that is
// not safe to create under a wiki's raw/ folder. Wrapped with the reason.
var ErrInvalidPath = errors.New("invalid path")

// ErrInvalidInput is returned for a request that is wrong in itself — nothing to ingest, a
// source that does not exist or matches nothing, one that overlaps the wiki, a limit exceeded,
// two files colliding. It is the caller's to fix, not a failure of the store. Wrapped with the
// reason; match it with errors.Is.
var ErrInvalidInput = errors.New("invalid input")

// ErrBusy is returned when another operation holds the wiki's lock. It carries
// what is running and since when, so a caller can tell a fresh operation from a
// long-running one.
type ErrBusy struct {
	Slug  string
	Op    string    // the operation holding the lock ("ingest", "query", …)
	Since time.Time // when that operation started; zero if unknown
}

func (e *ErrBusy) Error() string {
	msg := fmt.Sprintf("wiki %q is busy", e.Slug)
	if e.Op != "" {
		msg += " (" + e.Op
		if !e.Since.IsZero() {
			msg += ", " + roundAge(time.Since(e.Since))
		}
		msg += ")"
	}
	return msg + " — another agent is working on it, try again later"
}

// roundAge renders a duration coarsely: seconds under a minute, then minutes,
// then hours.
func roundAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

// WikiBusy marks the error for consumers that must recognise "the wiki is busy" without
// importing this package (the Wiki* tools match it through a one-method interface).
func (e *ErrBusy) WikiBusy() bool { return true }

// IsBusy reports whether err is an *ErrBusy.
func IsBusy(err error) bool {
	var b *ErrBusy
	return errors.As(err, &b)
}

// ConflictError is returned by AddSources when one or more destination files in
// raw/ already exist with DIFFERENT content. Nothing was written. Paths are
// relative to raw/ and use "/" as the separator.
type ConflictError struct {
	Paths []string
}

func (e *ConflictError) Error() string {
	const show = 20
	shown := e.Paths
	more := ""
	if len(shown) > show {
		more = fmt.Sprintf(" (and %d more)", len(shown)-show)
		shown = shown[:show]
	}
	return fmt.Sprintf("conflicts in raw/: %d file(s) already exist with different content and nothing was copied: %s%s",
		len(e.Paths), strings.Join(shown, ", "), more)
}
