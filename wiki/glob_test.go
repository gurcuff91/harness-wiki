package wiki

import "testing"

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		// single segment
		{"*.md", "a.md", true},
		{"*.md", "a.txt", false},
		{"*.md", "dir/a.md", false}, // * never crosses a separator
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"[abc].md", "b.md", true},
		{"[abc].md", "d.md", false},
		{"notes-*", "notes-2026", true},
		// exact
		{"a/b/c.md", "a/b/c.md", true},
		{"a/b/c.md", "a/b/d.md", false},
		{"a/b", "a/b/c", false},
		{"a/b/c", "a/b", false},
		// ** across segments
		{"docs/**/*.md", "docs/a.md", true}, // ** matches zero segments
		{"docs/**/*.md", "docs/x/a.md", true},
		{"docs/**/*.md", "docs/x/y/z/a.md", true},
		{"docs/**/*.md", "docs/x/a.txt", false},
		{"docs/**/*.md", "other/x/a.md", false},
		{"docs/**/*.md", "docs", false},
		{"**/*.pdf", "a.pdf", true},
		{"**/*.pdf", "x/y/a.pdf", true},
		{"**/*.pdf", "x/y/a.md", false},
		{"src/**", "src/a", true},
		{"src/**", "src/a/b/c", true},
		{"src/**", "src", true}, // a trailing ** matches nothing too
		{"src/**", "lib/a", false},
		{"**", "anything/at/all", true},
		{"**", "a", true},
		{"a/**/b/**/c", "a/b/c", true},
		{"a/**/b/**/c", "a/x/b/y/z/c", true},
		{"a/**/b/**/c", "a/x/c", false},
		{"a/**/**/b", "a/x/y/b", true}, // a run of ** segments is one **
		{"a/**/**/b", "a/b", true},
		// a bad pattern never matches (and never panics)
		{"[", "a", false},
		{"a/[", "a/b", false},
	}
	for _, c := range cases {
		if got := matchGlob(c.pattern, c.name); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestGlobRoot(t *testing.T) {
	cases := []struct{ pattern, root, rest string }{
		{"/data/docs/**/*.md", "/data/docs", "**/*.md"},
		{"/data/*.md", "/data", "*.md"},
		{"docs/**/*.md", "docs", "**/*.md"},
		{"*.md", ".", "*.md"},
		{"**/*.md", ".", "**/*.md"},
		{"/*.md", "/", "*.md"},
		{"a/b/c.md", "a/b/c.md", ""},
		{"/a/b", "/a/b", ""},
	}
	for _, c := range cases {
		root, rest := globRoot(c.pattern)
		if root != c.root || rest != c.rest {
			t.Errorf("globRoot(%q) = (%q, %q), want (%q, %q)", c.pattern, root, rest, c.root, c.rest)
		}
	}
}

func TestHasGlobMeta(t *testing.T) {
	for s, want := range map[string]bool{"a/b.md": false, "a/*.md": true, "a?": true, "a[1]": true, "plain": false, "": false} {
		if got := hasGlobMeta(s); got != want {
			t.Errorf("hasGlobMeta(%q) = %v, want %v", s, got, want)
		}
	}
}
