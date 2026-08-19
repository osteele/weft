package campaign

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"testing"
)

// oneSidedParamFields are assigned after CheckParams by some callers and
// deliberately not by others. Anything else appearing on one side only is the
// divergence this test exists to catch.
var oneSidedParamFields = map[string]string{
	"HedgeCohortHasReadySibling": "hedge culling is a fleet-level decision; the watcher sees one instance",
}

// TestCheckParamsCallerParity compares the CheckInstanceParams field sets the
// production callers populate after CheckParams. Everything shared now arrives
// as a required positional argument, so the losses that motivated this guard —
// the watcher's SetupSurvival and running-phase terminal-job fields — can no
// longer recur. What remains unenforced by the type system is post-call
// mutation of the returned struct: still legal Go, and still able to disable a
// rule on one path only. This test is the only thing standing against it.
//
// The scan covers the whole package rather than a list of filenames, following
// TestEffectiveThresholdRegistryIsExhaustive: relocating a caller, or adding a
// third one in a new file, must not carry it out of scope silently.
//
// Inertness guard: finding no CheckParams call site anywhere in the package
// means the scan has detached (rename, relocation) and fails loudly. Finding
// call sites with zero post-assignments is the correct post-refactor state,
// not inertness.
func TestCheckParamsCallerParity(t *testing.T) {
	callers, err := scanCheckParamsCallers()
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	if len(callers) == 0 {
		t.Fatal("found no CheckParams call site in the package — the scan matched nothing, and this guard is now inert")
	}

	assignedSomewhere := map[string]bool{}
	for _, fields := range callers {
		maps.Copy(assignedSomewhere, fields)
	}

	for _, file := range slices.Sorted(maps.Keys(callers)) {
		for _, field := range slices.Sorted(maps.Keys(assignedSomewhere)) {
			if callers[file][field] {
				continue
			}
			if _, ok := oneSidedParamFields[field]; ok {
				continue
			}
			t.Errorf("CheckInstanceParams.%s is assigned after CheckParams by another caller but not by the one in %s (callers: %s). Populate it inside CheckParams so every caller gets it, or — if it is genuinely one-sided — add it to oneSidedParamFields in checkparams_parity_test.go with a reason; existing entries: %v.",
				field, file, strings.Join(slices.Sorted(maps.Keys(callers)), ", "), oneSidedParamFields)
		}
	}
}

// scanCheckParamsCallers parses every non-test file of this package and
// returns, per file that calls CheckParams, the names of fields subsequently
// assigned on the local(s) holding the result. A file with a call site but no
// assignments maps to an empty set, which is how a caller that adds nothing
// stays visible to the comparison.
func scanCheckParamsCallers() (map[string]map[string]bool, error) {
	fset := token.NewFileSet()
	nonTest := func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }
	pkgs, err := parser.ParseDir(fset, ".", nonTest, 0)
	if err != nil {
		return nil, err
	}

	callers := map[string]map[string]bool{}
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			name := path
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				// The locals holding CheckParams results in this function.
				paramsIdents := map[string]bool{}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					assign, ok := n.(*ast.AssignStmt)
					if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
						return true
					}
					ident, ok := assign.Lhs[0].(*ast.Ident)
					if !ok {
						return true
					}
					call, ok := assign.Rhs[0].(*ast.CallExpr)
					if !ok {
						return true
					}
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "CheckParams" {
						paramsIdents[ident.Name] = true
					}
					return true
				})
				if len(paramsIdents) == 0 {
					continue
				}
				if callers[name] == nil {
					callers[name] = map[string]bool{}
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					assign, ok := n.(*ast.AssignStmt)
					if !ok {
						return true
					}
					for _, lhs := range assign.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok {
							if base, ok := sel.X.(*ast.Ident); ok && paramsIdents[base.Name] {
								callers[name][sel.Sel.Name] = true
							}
						}
					}
					return true
				})
			}
		}
	}
	return callers, nil
}
