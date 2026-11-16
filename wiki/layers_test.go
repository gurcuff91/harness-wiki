package wiki

import (
	"os/exec"
	"strings"
	"testing"
)

// The store is pure persistence: it must depend on neither the harness SDK (no model, no agent,
// no provider can reach it) nor any other package of this module (the HTTP API and the maintainer
// sit ABOVE it). Otherwise AI could leak into the store, and the package could not be reused on
// its own. The check is on the real dependency graph, so it fails the build, not a style rule.
func TestPackageHasNoForbiddenDependencies(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/gurcuff91/harness-wiki/wiki").CombinedOutput()
	if err != nil {
		t.Skipf("go list unavailable: %v\n%s", err, out)
	}
	const self = "github.com/gurcuff91/harness-wiki/wiki"
	for _, dep := range strings.Fields(string(out)) {
		switch {
		case dep == self:
		case strings.HasPrefix(dep, "github.com/gurcuff91/harness"):
			t.Errorf("wiki depends on %s — the store must stay free of the SDK and of the rest of this module", dep)
		}
	}
}
