package campaign

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// axisEnforcement claims which axes are re-checked locally, but a claim about
// "the pipeline" is only true if every ranking path runs that pipeline. It was
// not: rankOfferWithPredictedRuntime — the relaunch and autopilot replacement
// path — ran four of nine filters and asserted `stats.AfterVRAM = len(offers)`
// on the strength of the offers being provider-filtered. So the SKU axis was
// "checkedLocally" on one path and unchecked on the other, and a replacement
// rental could still be bought against the wrong part.
//
// TestConstraintAxisCoverage cannot catch that: it checks the map is complete,
// not that the filters actually run. This does, by asserting every ranking
// path delegates to the one shared chain rather than assembling its own.
func TestRankingPathsShareTheEligibilityChain(t *testing.T) {
	fset := token.NewFileSet()
	pkg, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	// Ranking entry points that must not filter offers themselves.
	rankers := map[string]bool{
		"rankOfferWithProfile":          true,
		"rankOfferWithPredictedRuntime": true,
	}
	for _, f := range pkg {
		for _, file := range f.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok || !rankers[fn.Name.Name] {
					return true
				}
				var callsShared bool
				var ownFilters []string
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					ident, ok := call.Fun.(*ast.Ident)
					if !ok {
						return true
					}
					switch {
					case ident.Name == "applyEligibilityFilters":
						callsShared = true
					case strings.HasPrefix(ident.Name, "filterOffersBy"):
						ownFilters = append(ownFilters, ident.Name)
					}
					return true
				})
				if !callsShared {
					t.Errorf("%s does not call applyEligibilityFilters; a ranking path that "+
						"assembles its own chain silently drops whatever it omits", fn.Name.Name)
				}
				if len(ownFilters) > 0 {
					t.Errorf("%s calls %v directly instead of through applyEligibilityFilters; "+
						"axisEnforcement's claims are only true if every path runs the same chain",
						fn.Name.Name, ownFilters)
				}
				return false
			})
		}
	}
}
