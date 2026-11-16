package maintainer

import "testing"

// isolate gives the test empty in-memory settings and credentials, so nothing in the developer's
// ~/.harness — a connected provider, an active model — can reach the code under test. The SDK
// reads those two stores lazily and process-wide, so they are installed before anything else.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "MINIMAX_API_KEY", "OLLAMA_CLOUD_API_KEY", "OPENCODE_GO_API_KEY", "OLLAMA_URL"} {
		t.Setenv(k, "")
	}
	// The stores are process-wide and can be registered only before the first Agent is built, so
	// this package does it ONCE, in TestMain (see main_test.go).
}
