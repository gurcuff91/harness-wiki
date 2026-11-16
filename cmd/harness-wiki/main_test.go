package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseServeDefaults(t *testing.T) {
	c, err := parseServe(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if c.host != "127.0.0.1" || c.port != 8787 || c.model != "" {
		t.Fatalf("defaults = %+v, want 127.0.0.1:8787 and no model (the first available is used)", c)
	}
}

func TestParseServeFlags(t *testing.T) {
	c, err := parseServe([]string{"--host", "0.0.0.0", "--port", "9090", "--model", "anthropic/claude-sonnet-5-5"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if c.host != "0.0.0.0" || c.port != 9090 || c.model != "anthropic/claude-sonnet-5-5" {
		t.Fatalf("config = %+v", c)
	}
	// both spellings, and = form
	c, err = parseServe([]string{"-port=1234", "--host=::1"}, &bytes.Buffer{})
	if err != nil || c.port != 1234 || c.host != "::1" {
		t.Fatalf("config = %+v, %v", c, err)
	}
}

func TestParseServeRejectsBadInput(t *testing.T) {
	for _, args := range [][]string{
		{"--port", "-1"}, {"--port", "65536"}, {"--port", "abc"},
		{"--nope"}, {"extra"}, {"--model"},
	} {
		if _, err := parseServe(args, &bytes.Buffer{}); err == nil {
			t.Errorf("parseServe(%v) accepted", args)
		}
	}
}

func TestRunCommands(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, &out, &errb); code != 2 || !strings.Contains(errb.String(), "Usage") {
		t.Errorf("no args: code=%d stderr=%q, want 2 and the usage", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := run([]string{"version"}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "harness-wiki ") {
		t.Errorf("version: code=%d out=%q", code, out.String())
	}
	out.Reset()
	if code := run([]string{"help"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "--host") || !strings.Contains(out.String(), "0.0.0.0") || !strings.Contains(out.String(), "./harness-wiki/") {
		t.Errorf("help: code=%d out=%q — it must document --host, --port, --model and where the wikis live", code, out.String())
	}
	errb.Reset()
	if code := run([]string{"bogus"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), `unknown command "bogus"`) {
		t.Errorf("unknown command: code=%d stderr=%q", code, errb.String())
	}
	errb.Reset()
	if code := run([]string{"serve", "--port", "99999"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "not a valid port") {
		t.Errorf("a bad port: code=%d stderr=%q", code, errb.String())
	}
}

// The usage text must state the facts a user needs: it is the only documentation `--help` shows.
func TestUsageStatesTheDefaults(t *testing.T) {
	for _, want := range []string{"127.0.0.1", "8787", "--model", "first model", "./harness-wiki/"} {
		if !strings.Contains(usage, want) {
			t.Errorf("the usage lacks %q", want)
		}
	}
}
