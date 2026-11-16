package wiki

import (
	"errors"
	"strings"
	"testing"
)

func TestValidSlug(t *testing.T) {
	valid := []string{"a", "x1", "chrysler-sgw", "a-b-c", "0", "2026-notes", strings.Repeat("a", 64), "com10", "console", "nullable", "auxiliary"}
	for _, s := range valid {
		if err := ValidSlug(s); err != nil {
			t.Errorf("ValidSlug(%q) = %v, want ok", s, err)
		}
	}
	invalid := map[string]string{
		"":                      "empty",
		"A":                     "uppercase",
		"Chrysler":              "uppercase",
		"a b":                   "space",
		"a_b":                   "underscore",
		"a.b":                   "dot",
		"..":                    "dots",
		"../x":                  "traversal",
		"a/b":                   "separator",
		`a\b`:                   "backslash",
		"/etc":                  "absolute",
		"-a":                    "leading hyphen",
		"a-":                    "trailing hyphen",
		"-":                     "only a hyphen",
		strings.Repeat("a", 65): "too long",
		"con":                   "reserved",
		"nul":                   "reserved",
		"aux":                   "reserved",
		"prn":                   "reserved",
		"com1":                  "reserved",
		"com9":                  "reserved",
		"lpt1":                  "reserved",
		"lpt9":                  "reserved",
		"ñandú":                 "non-ascii",
		"a\x00b":                "nul byte",
		"a\nb":                  "newline",
		" a":                    "leading space",
		"a:b":                   "colon",
	}
	for s, why := range invalid {
		err := ValidSlug(s)
		if err == nil {
			t.Errorf("ValidSlug(%q) accepted (%s)", s, why)
			continue
		}
		if !errors.Is(err, ErrInvalidSlug) {
			t.Errorf("ValidSlug(%q) error %v does not wrap ErrInvalidSlug", s, err)
		}
	}
	// the error teaches the valid shape
	if err := ValidSlug("Bad Slug"); err == nil || !strings.Contains(err.Error(), "lowercase letters, digits and hyphens") {
		t.Errorf("error should state the valid shape: %v", err)
	}
}

func TestValidRelPath(t *testing.T) {
	ok := map[string]string{
		"data.md":                "data.md",
		"ctx/data.md":            "ctx/data.md",
		`ctx\data.md`:            "ctx/data.md", // a backslash is a separator too
		"a/b/c/d.txt":            "a/b/c/d.txt",
		"notes (1).md":           "notes (1).md",
		"Résumé.md":              "Résumé.md",
		"console/x.md":           "console/x.md", // "console" is not "con"
		"v1.2/readme":            "v1.2/readme",
		".hidden":                ".hidden",
		"dir/.gitkeep":           "dir/.gitkeep",
		strings.Repeat("a", 200): strings.Repeat("a", 200),
	}
	for in, want := range ok {
		got, err := ValidRelPath(in)
		if err != nil || got != want {
			t.Errorf("ValidRelPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{
		"", "   ", "/", `\`,
		"/etc/passwd", `\windows`, `\\server\share\x`,
		"../x", "a/../../b", "a/..", "..", ".", "a/./b",
		"C:/x", `C:\x`, "c:x", "a/b:c",
		"a//b", `a\\b`, "a/\\b", "a/",
		"a<b", "a>b", `a"b`, "a|b", "a?b", "a*b",
		"a\x00b", "a\nb", "a\x1fb", "a\x7fb",
		"a. ", "a.", "dir./x", "dir /x", "x ",
		"con", "CON", "con.txt", "dir/nul", "dir/NUL.md", "com1.tar.gz", "a/lpt3/b",
		strings.Repeat("a", 256), strings.Repeat("a/", 101),
	}
	for _, in := range bad {
		got, err := ValidRelPath(in)
		if err == nil {
			t.Errorf("ValidRelPath(%q) = %q, want an error", in, got)
			continue
		}
		if !errors.Is(err, ErrInvalidPath) {
			t.Errorf("ValidRelPath(%q) error %v does not wrap ErrInvalidPath", in, err)
		}
	}
}

// Whatever ValidRelPath accepts must stay inside its folder when joined — the
// property the validation exists for.
func TestValidRelPathNeverEscapes(t *testing.T) {
	inputs := []string{"a", "a/b", "./a", "a/../b", "../a", "a/../../b", "a/b/../../../c", `..\a`, `a\..\..\b`, "%2e%2e/a", "....//a", "a/...", ".../a"}
	for _, in := range inputs {
		clean, err := ValidRelPath(in)
		if err != nil {
			continue
		}
		if strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") || strings.HasSuffix(clean, "/..") {
			t.Errorf("ValidRelPath(%q) = %q escapes", in, clean)
		}
	}
}

// Each rule must reject on its OWN: they overlap on purpose (".." also ends in a dot, "/x"
// also starts with an empty segment), and a defence that only works because another one
// happens to catch the same input is not a defence. Pin the reason, not just the refusal.
func TestEachPathRuleRejectsOnItsOwn(t *testing.T) {
	cases := []struct{ name, reason string }{
		{"../x", `".." segments are not allowed`},
		{"a/../b", `".." segments are not allowed`},
		{"a/./b", `"." segments are not allowed`},
		{"/abs", "must be relative"},
		{`\win`, "must be relative"},
		{"a//b", "empty path segment"},
		{"a/", "empty path segment"},
		{"a. ", "must not end with a dot or a space"},
		{"con.txt", "reserved name"},
		{"a<b", "not allowed on every platform"},
		{"", "empty"},
		{strings.Repeat("a", 256), "longer than 255"},
		{strings.Repeat("a", 100) + "/" + strings.Repeat("b", 101), "maximum is 200"},
	}
	for _, c := range cases {
		_, err := ValidRelPath(c.name)
		if err == nil || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("ValidRelPath(%q) = %v, want a refusal for %q", c.name, err, c.reason)
		}
	}
}
