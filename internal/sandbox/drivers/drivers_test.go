// SPDX-License-Identifier: MIT

package drivers

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The container option that lets a driver run against a rootless Docker daemon
// exists for tests: with it the nested dockerd of the job cannot start, so jobs
// would run without Docker and with weaker isolation than the runner promises.
// Production code (this package builds the drivers of `ermite-runner run`) must
// never mention it, neither as a field of Options nor in a value.
func TestProductionFactoriesNeverSetTheTestOnlyRootlessOption(t *testing.T) {
	fset := token.NewFileSet()
	files, err := parser.ParseFile(fset, "drivers.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(files, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			if x.Name == "UnsafeAllowRootlessForTests" || x.Name == "AllowRootless" {
				t.Errorf("%s: %s is a test-only option", fset.Position(x.Pos()), x.Name)
			}
		case *ast.CompositeLit:
			if sel, ok := x.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Options" {
				found = true
				if len(x.Elts) != 0 {
					t.Errorf("%s: %s{...} sets options in production code", fset.Position(x.Pos()), sel.Sel.Name)
				}
			}
		}
		return true
	})
	if !found {
		t.Fatal("drivers.go no longer builds container.Options{}: update this test")
	}
}
