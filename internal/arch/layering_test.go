// Package arch holds no production code. It exists only to assert, at test time,
// that codamigo's internal package dependencies match the one-way order
// documented in AGENTS.md.
//
// A true import cycle is already a compile error in Go, so one can never ship.
// What this test catches is the layering violation that compiles fine — `store`
// reaching for `chunker`, or `localembed` reaching for `config` — which is what
// makes "config is passed at construction time" mechanically true instead of a
// convention someone remembers.
package arch_test

import (
	"go/build"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// allowed maps each internal package to the internal packages it may import.
// Mirrors the dependency order documented in AGENTS.md. Verified against
// `go list` at the time of writing — an entry here grants permission, so keep
// each list as small as the code actually needs rather than as wide as the
// diagram suggests.
//
// Note that mcp does NOT import config: it receives what it needs through
// mcp.WithXxx options. Adding "config" here would loosen the rule, not enforce it.
var allowed = map[string][]string{
	"config": {},
	"store":  {},
	// localembed must never import config: that is what keeps "config is passed
	// at construction time" true for the local embedding provider rather than a
	// convention someone remembers.
	"localembed": {},
	"walker":     {"config"},
	"watcher":    {"config"},
	"indexer":    {"store", "walker"},
	"query":      {"store"},
	"mcp":        {"indexer", "query", "store", "watcher"},
}

// exempt packages are not subject to the allow-list.
//   - cmd/codamigo is the single wiring point and may import anything.
//   - internal/arch is this test itself.
var exempt = []string{
	"cmd/codamigo",
	"internal/arch",
}

func TestInternalPackageLayering(t *testing.T) {
	root := repoRoot(t)
	modulePath := modulePath(t, root)

	// UseAllFiles ignores GOOS/GOARCH build constraints. Without it, ImportDir
	// only reports the files that build on the current platform, so a violation
	// in watcher/backend_inotify.go would be invisible when running on darwin.
	// Every file in a given directory here declares the same package, so the
	// conflicting-package-name caveat of UseAllFiles does not bite.
	ctxt := build.Default
	ctxt.UseAllFiles = true

	pkgs := internalPackages(t, root)
	if len(pkgs) == 0 {
		t.Fatal("found no internal packages to check; repo layout or root detection changed")
	}

	for _, rel := range pkgs {
		t.Run(rel, func(t *testing.T) {
			pkg, err := ctxt.ImportDir(filepath.Join(root, rel), 0)
			if err != nil {
				t.Fatalf("reading imports of %s: %v", rel, err)
			}

			// pkg.Imports covers GoFiles + CgoFiles only. Test files land in
			// TestImports/XTestImports and are deliberately not checked, which
			// preserves the existing pattern of a _test.go importing a parent
			// package for a compile-time interface assertion.
			deps := ownModuleImports(pkg.Imports, modulePath)

			permitted, ok := allowed[rel]
			if !ok {
				t.Fatalf("package %q is not listed in the allow-list in %s.\n"+
					"Add it (with the smallest set of imports it needs) and update the "+
					"dependency order in AGENTS.md to match.", rel, "internal/arch/layering_test.go")
			}

			for _, dep := range deps {
				if !slices.Contains(permitted, dep) {
					t.Errorf("illegal import: %q imports %q.\n"+
						"  %q may import: %v\n"+
						"See the one-way dependency order in AGENTS.md. If this edge is "+
						"genuinely correct, widen the allow-list and the AGENTS.md diagram "+
						"together — do not widen only the test.",
						rel, dep, rel, permitted)
				}
			}
		})
	}
}

// TestAllowListHasNoStaleEntries fails when the allow-list names a package that
// no longer exists, so a rename cannot silently leave a permission behind that
// grants more than the real layout needs.
func TestAllowListHasNoStaleEntries(t *testing.T) {
	root := repoRoot(t)
	present := internalPackages(t, root)

	for name := range allowed {
		if !slices.Contains(present, name) {
			t.Errorf("allow-list entry %q has no corresponding package directory; "+
				"remove it from internal/arch/layering_test.go", name)
		}
	}
}

// internalPackages returns the module-relative directories that contain Go
// package sources, excluding exempt paths, testdata, and hidden directories.
func internalPackages(t *testing.T, root string) []string {
	t.Helper()

	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata") {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if slices.Contains(exempt, rel) {
			return filepath.SkipDir
		}
		if hasNonTestGoFiles(t, path) {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	slices.Sort(out)
	return out
}

func hasNonTestGoFiles(t *testing.T, dir string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			return true
		}
	}
	return false
}

// ownModuleImports reduces an import list to the module-relative paths of
// imports from this module, dropping stdlib and third-party imports.
//
// Paths are kept whole, so a sub-package is its own allow-list entry rather than
// inheriting its parent's: adding localembed/internal would need its own line in
// allowed, and importing it would need naming there explicitly.
func ownModuleImports(imports []string, modulePath string) []string {
	prefix := modulePath + "/"
	var out []string
	for _, imp := range imports {
		if !strings.HasPrefix(imp, prefix) {
			continue
		}
		rel := strings.TrimPrefix(imp, prefix)
		if !slices.Contains(out, rel) {
			out = append(out, rel)
		}
	}
	slices.Sort(out)
	return out
}

// repoRoot returns the module root, two levels up from internal/arch.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected go.mod at %s: %v", root, err)
	}
	return root
}

// modulePath reads the module path from go.mod without pulling in x/mod.
func modulePath(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(after)
		}
	}
	t.Fatalf("no module directive found in go.mod")
	return ""
}
