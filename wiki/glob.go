package wiki

import (
	"path"
	"strings"
)

// matchGlob reports whether name matches pattern. Both use "/" as the separator.
//
// It supports what path.Match supports within one segment — "*" (any run of
// characters except "/"), "?" (one character) and "[...]" classes — plus "**" as a
// whole segment, which matches zero or more whole segments:
//
//	docs/**/*.md   matches docs/a.md, docs/x/a.md, docs/x/y/a.md
//	**/*.pdf       matches a.pdf and any/depth/a.pdf
//	src/**         matches everything under src/
//
// The standard library's filepath.Glob has no "**", and adding a dependency for one
// function is not worth it.
func matchGlob(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Collapse a run of ** (they mean the same as one).
			for len(pat) > 1 && pat[1] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 1 {
				return true // a trailing ** swallows whatever is left
			}
			// Try the rest of the pattern against every possible suffix of name.
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], name[0])
		if err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// hasGlobMeta reports whether s contains a glob metacharacter. A path without one is
// taken literally, so a file called "notes [draft].md" can still be named as a plain
// path as long as it exists.
func hasGlobMeta(s string) bool {
	return strings.ContainsAny(s, "*?[")
}

// globRoot returns the longest leading run of pattern segments that contain no
// metacharacter — the directory a glob can start walking from — and the remaining
// pattern relative to it. For "/data/docs/**/*.md" it is ("/data/docs", "**/*.md").
func globRoot(pattern string) (root, rest string) {
	segs := strings.Split(pattern, "/")
	i := 0
	for i < len(segs) && !hasGlobMeta(segs[i]) {
		i++
	}
	// A pattern whose first segment has a metacharacter starts at the current
	// directory; a rooted pattern keeps its leading "".
	root = strings.Join(segs[:i], "/")
	if root == "" && strings.HasPrefix(pattern, "/") {
		root = "/"
	}
	if root == "" {
		root = "."
	}
	return root, strings.Join(segs[i:], "/")
}
