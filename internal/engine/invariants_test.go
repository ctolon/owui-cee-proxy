package engine_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestInvariant_NoNameChecksInAdapters enforces CLAUDE.md invariant
// #6 ("Capabilities, not name checks. Code that varies behaviour by
// engine MUST query eng.Capabilities() — never eng.Name() ==").
// We parse every Go file under internal/engine/compat/ AND
// internal/transport/http/handlers/ and reject:
//
//   - Comparison expressions whose LHS is `something.Name()` call.
//   - Comparison expressions against an explicit engine name
//     literal ("docling", "tika", "external", "kreuzberg",
//     "docling-external") on either side.
//
// Behaviour-by-name leaks are silent regressions that don't break
// tests — until a new operator names their engine something else.
// This test catches them at CI time.
func TestInvariant_NoNameChecksInAdapters(t *testing.T) {
	t.Parallel()
	roots := []string{
		"compat",
		filepath.Join("..", "transport", "http", "handlers"),
		filepath.Join("..", "transport", "http", "reverseproxy"),
	}
	bannedLiterals := map[string]struct{}{
		`"docling"`:          {},
		`"tika"`:             {},
		`"external"`:         {},
		`"kreuzberg"`:        {},
		`"docling-external"`: {},
	}

	for _, root := range roots {
		fset := token.NewFileSet()
		// parser.ParseDir is "deprecated since Go 1.25" in favour of
		// golang.org/x/tools/go/packages; that alternative parses with
		// build-tag awareness and full type info. For this invariant
		// guard we explicitly want the FILE list at the root and
		// nothing else (no transitive deps, no type info), so the
		// simpler API stays the right tool.
		pkgs, err := parser.ParseDir(fset, root, func(info fs.FileInfo) bool { //nolint:staticcheck // SA1019: see comment above.
			return !strings.HasSuffix(info.Name(), "_test.go")
		}, parser.ParseComments)
		require.NoError(t, err, "parse %s", root)

		for _, pkg := range pkgs {
			for path, f := range pkg.Files {
				ast.Inspect(f, func(n ast.Node) bool {
					bin, ok := n.(*ast.BinaryExpr)
					if !ok {
						return true
					}
					if bin.Op != token.EQL && bin.Op != token.NEQ {
						return true
					}
					if isNameCallExpr(bin.X) || isNameCallExpr(bin.Y) {
						pos := fset.Position(bin.Pos())
						t.Errorf("%s:%d: CLAUDE.md invariant #6: do not compare Name() values; use Capabilities()",
							path, pos.Line)
					}
					for _, side := range []ast.Expr{bin.X, bin.Y} {
						lit, ok := side.(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						if _, banned := bannedLiterals[lit.Value]; banned {
							other := bin.Y
							if side == bin.Y {
								other = bin.X
							}
							if isLikelyEngineName(other) {
								pos := fset.Position(bin.Pos())
								t.Errorf("%s:%d: CLAUDE.md invariant #6: do not gate on engine-name literal %s; use Capabilities()",
									path, pos.Line, lit.Value)
							}
						}
					}
					return true
				})
			}
		}
	}
}

// isNameCallExpr returns true when expr looks like `x.Name()`.
func isNameCallExpr(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	return sel.Sel.Name == "Name"
}

// isLikelyEngineName tries to identify expressions that are
// engine-name-typed. Conservative: catches `engine.Name(x)`,
// `e.Name()`, plain identifiers ending in "Name". This is a heuristic
// (full type resolution would need go/types).
func isLikelyEngineName(expr ast.Expr) bool {
	switch x := expr.(type) {
	case *ast.CallExpr:
		// engine.Name("docling") conversion is fine; we look for .Name() calls.
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Name" {
			return true
		}
	case *ast.Ident:
		return strings.HasSuffix(x.Name, "Name") || x.Name == "name"
	case *ast.SelectorExpr:
		return x.Sel.Name == "Name" || x.Sel.Name == "name"
	}
	return false
}
