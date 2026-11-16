package maintainer

import (
	"fmt"
	"os"
	"testing"

	"github.com/gurcuff91/harness/agent"
	"github.com/gurcuff91/harness/configstore"
)

// TestMain installs empty in-memory settings and credentials stores before ANY agent exists, so
// no test can read the developer's real ~/.harness (a connected provider, an active model, an
// OAuth login). The SDK refuses to swap a store once an agent has used it, hence once, here.
func TestMain(m *testing.M) {
	if err := agent.SetSettingsStore(configstore.NewInMemoryStore()); err != nil {
		fmt.Fprintln(os.Stderr, "settings store:", err)
		os.Exit(1)
	}
	if err := agent.SetCredentialsStore(configstore.NewInMemoryStore()); err != nil {
		fmt.Fprintln(os.Stderr, "credentials store:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
