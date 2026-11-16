package wiki

import (
	"fmt"
	"regexp"
	"strings"
)

// maxSlugLen is the longest slug accepted.
const maxSlugLen = 64

// slugRe is the portable shape of a slug: lowercase ASCII letters and digits,
// with single hyphens allowed inside — never at either end. There are no dots
// and no separators, so "../x" is impossible by construction, and being
// lowercase-only means two slugs can never differ only by case (the same folder
// on macOS and Windows).
var slugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// reservedNames are the device names Windows will not allow as a file or folder
// name — with or without an extension ("con.txt" is just as invalid as "con").
var reservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// isReserved reports whether a single path segment is a Windows device name,
// ignoring case and any extension ("CON", "nul.txt", "Com1.tar.gz").
func isReserved(segment string) bool {
	base := segment
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	return reservedNames[strings.ToLower(base)]
}

// ValidSlug checks that s can name a wiki: 1–64 characters of [a-z0-9-], not
// starting or ending with a hyphen, and not a reserved Windows device name.
// The error wraps ErrInvalidSlug and says what a valid slug looks like.
func ValidSlug(s string) error {
	const shape = "use 1-64 lowercase letters, digits and hyphens, not starting or ending with a hyphen (e.g. \"chrysler-sgw\")"
	switch {
	case s == "":
		return fmt.Errorf("%w: slug is empty — %s", ErrInvalidSlug, shape)
	case len(s) > maxSlugLen:
		return fmt.Errorf("%w: %q is %d characters, the maximum is %d — %s", ErrInvalidSlug, s, len(s), maxSlugLen, shape)
	case !slugRe.MatchString(s):
		return fmt.Errorf("%w: %q — %s", ErrInvalidSlug, s, shape)
	case isReserved(s):
		return fmt.Errorf("%w: %q is a reserved name on Windows — choose another", ErrInvalidSlug, s)
	}
	return nil
}

const (
	maxSegmentLen = 255 // the per-segment limit of every filesystem we ship on
	maxRelPathLen = 200 // leaves margin under Windows' classic 260-character path limit
)

// splitSegments splits on both "/" and "\\", keeping empty segments.
func splitSegments(name string) []string {
	var segs []string
	start := 0
	for i, r := range name {
		if r == '/' || r == '\\' {
			segs = append(segs, name[start:i])
			start = i + 1
		}
	}
	return append(segs, name[start:])
}

// ValidRelPath validates and normalises a relative file path — the name of an
// inline file — so that it can be created under a wiki's raw/ folder without
// escaping it, on any platform we ship. It splits on both "/" and "\" and
// returns the cleaned path joined with "/".
//
// Rejected: empty names; absolute paths, drive letters ("C:") and UNC paths;
// empty, "." and ".." segments; the characters < > : " | ? * and control
// characters; segments ending in a dot or a space; Windows device names even
// with an extension; segments over 255 characters; and a total length over 200.
// Every error wraps ErrInvalidPath.
func ValidRelPath(name string) (string, error) {
	bad := func(format string, a ...any) (string, error) {
		return "", fmt.Errorf("%w: %q: %s", ErrInvalidPath, name, fmt.Sprintf(format, a...))
	}
	if strings.TrimSpace(name) == "" {
		return bad("the name is empty")
	}
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) {
		return bad("it must be relative to raw/, not absolute")
	}
	// Split on BOTH separators, keeping empty segments so that "a//b", "a/\b" and a
	// trailing "/" are reported instead of being silently normalised.
	segments := splitSegments(name)
	for _, seg := range segments {
		if seg == "" {
			return bad("it has an empty path segment")
		}
	}
	for _, seg := range segments {
		switch {
		case seg == "." || seg == "..":
			return bad("%q segments are not allowed", seg)
		case len(seg) > maxSegmentLen:
			return bad("a segment is longer than %d characters", maxSegmentLen)
		case strings.TrimRight(seg, ". ") != seg:
			return bad("a segment must not end with a dot or a space (%q)", seg)
		case isReserved(seg):
			return bad("%q is a reserved name on Windows", seg)
		}
		for _, r := range seg {
			if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"|?*`, r) {
				return bad("a segment contains a character that is not allowed on every platform (%q)", r)
			}
		}
	}
	clean := strings.Join(segments, "/")
	if len(clean) > maxRelPathLen {
		return bad("it is %d characters, the maximum is %d", len(clean), maxRelPathLen)
	}
	return clean, nil
}
