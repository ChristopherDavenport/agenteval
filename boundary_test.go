package agenteval_test

import (
	"go/build"
	"strings"
	"testing"
)

// allowed are the module prefixes the root module may import beyond
// the standard library: the four siblings, and itself.
var allowed = []string{
	"github.com/ChristopherDavenport/agenteval",
	"github.com/ChristopherDavenport/agentsession",
	"github.com/ChristopherDavenport/agenttool",
	"github.com/ChristopherDavenport/agentturn",
	"github.com/ChristopherDavenport/openresponses",
}

// TestImportBoundary enforces the rule that the root module builds
// from the siblings and the standard library alone, so anything that
// needs another dependency is a nested module. The deps target checks
// the whole build graph; this checks what the packages themselves say.
func TestImportBoundary(t *testing.T) {
	for _, dir := range []string{".", "replay", "judge", "price"} {
		pkg, err := build.Default.ImportDir(dir, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range append(pkg.Imports, pkg.TestImports...) {
			if !strings.Contains(imp, ".") {
				continue // standard library
			}
			ok := false
			for _, prefix := range allowed {
				if imp == prefix || strings.HasPrefix(imp, prefix+"/") {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("%s imports %q; only the siblings and the standard library are allowed", pkg.ImportPath, imp)
			}
		}
	}
}
