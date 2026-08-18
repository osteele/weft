package campaign

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

// Every effective* method on CheckInstanceParams lets a learned survival
// threshold stand in for a hardcoded window. Three times a method has
// borrowed a curve that measures a different event than the rule adjudicates,
// and the window silently grew by an order of magnitude:
//
//   - dud detection took bootstrap-completion for probe-arrival and became
//     110 minutes; instance wi7309 billed 86 minutes and $3.80 (ADR 0008).
//   - empty-status and stale-non-running took the same curve for two
//     pre-running events it does not overlap at all (ADR 0010).
//
// Nothing required a new learned-threshold consumer to say whether its
// borrowed curve is bounded, so the dud case reached production and cost a
// rental before anyone looked, and the two in ADR 0010 surfaced only because
// someone re-asked 0008's question by hand across the remaining consumers.
// This registry is that requirement, mechanized. Every effective* method must
// appear below with either a ceiling or an explicit exemption, and
// TestEffectiveThresholdRegistryIsExhaustive fails when one does not — so a
// seventh consumer cannot be added without recording the decision.
//
// Scope: effective* methods on CheckInstanceParams. Watchdogs that read
// LearnedTerminate() inline — the bootstrap and setup-stall deadlines in
// instance_check.go, and the persisted bootstrap deadlines in sync_state.go
// and lifecycle.go — are outside it. Extracting those into methods is what
// would bring them under the guard.
//
// This is the machine-checked half of ADRs 0005, 0008, and 0010. Changing an
// entry here means changing one of those, which means writing a new record
// rather than editing an accepted one.
type thresholdPolicy struct {
	// method is the method name as declared, matched against the source.
	method string
	// eval calls it. Unexported methods are invisible to reflection, so the
	// binding is explicit.
	eval func(CheckInstanceParams) time.Duration
	// learned builds params on which the survival curve this method borrows
	// reports the given terminate threshold. The constructor identifies the
	// curve, so entries sharing a distribution share a constructor.
	learned func(time.Duration) CheckInstanceParams
	// fallback is the constant the method returns when nothing was learned.
	fallback time.Duration
	// ceiling bounds the learned value. Zero iff exempt.
	ceiling time.Duration
	// exempt records that the learned value is deliberately unbounded.
	exempt bool
	// adr is the record carrying the decision.
	adr string
	// why is required for an exemption and must say why unbounded is safe.
	why string
}

// learnedBootstrap and learnedSetup set only Terminate: LearnedTerminate reads
// Terminate.Learned() and nothing else, so a SampleSize or Warn here would
// imply a gate that does not exist on this path.

func learnedBootstrap(d time.Duration) CheckInstanceParams {
	return CheckInstanceParams{
		BootstrapSurvival: &db.BootstrapSurvival{Terminate: db.LearnedThreshold(d)},
	}
}

func learnedSetup(d time.Duration) CheckInstanceParams {
	return CheckInstanceParams{
		SetupSurvival: &db.SetupSurvival{Terminate: db.LearnedThreshold(d)},
	}
}

var thresholdPolicies = []thresholdPolicy{
	{
		method:   "effectiveMaxEmptyStatusTime",
		eval:     CheckInstanceParams.effectiveMaxEmptyStatusTime,
		learned:  learnedBootstrap,
		fallback: maxEmptyStatusTime,
		ceiling:  maxEmptyStatusTimeCeiling,
		adr:      "0010",
	},
	{
		method:   "effectiveMaxPreRunningStatusTime",
		eval:     CheckInstanceParams.effectiveMaxPreRunningStatusTime,
		learned:  learnedBootstrap,
		fallback: maxPreRunningStatusTime,
		ceiling:  maxPreRunningStatusTimeCeiling,
		adr:      "0010",
	},
	{
		method:   "effectiveDudVastTimeout",
		eval:     CheckInstanceParams.effectiveDudVastTimeout,
		learned:  learnedBootstrap,
		fallback: dudVastTimeout,
		ceiling:  dudVastTimeoutCeiling,
		adr:      "0008",
	},
	{
		method:   "effectiveLaunchingPhaseTimeout",
		eval:     CheckInstanceParams.effectiveLaunchingPhaseTimeout,
		learned:  learnedBootstrap,
		fallback: launchingPhaseTimeout,
		exempt:   true,
		adr:      "0010",
		why: "Failure direction is over-permissive, not false-killing: a wedged " +
			"pre-launch rental burns the window rather than a healthy one being " +
			"reaped. Every launch it governs lacks a BootstrapOrigin, so no " +
			"measured distribution anchors a ceiling. Clamping needs its own " +
			"measurement, not a borrowed one.",
	},
	{
		method:   "effectiveOnStartStallTimeout",
		eval:     CheckInstanceParams.effectiveOnStartStallTimeout,
		learned:  learnedSetup,
		fallback: onStartStallTimeout,
		exempt:   true,
		adr:      "0005",
		why: "ADR 0005 accepted the unbounded borrow on cost, not on fit. The " +
			"curve is blind to the looping-OnStart mode these windows catch — a " +
			"loop never hands off to bootstrap.sh, so it contributes no setup " +
			"observations — but false kills are the costlier error, and the " +
			"worst-case bleed is roughly 35 minutes over the constant, cents at " +
			"interruptible rates. A threshold learned above the constant says " +
			"that project's own setups genuinely run that long. A dedicated " +
			"probe-to-handoff curve is the recorded long-term fix.",
	},
	{
		method:   "effectiveOnStartTotalActiveTimeout",
		eval:     CheckInstanceParams.effectiveOnStartTotalActiveTimeout,
		learned:  learnedSetup,
		fallback: onStartTotalActiveTimeout,
		exempt:   true,
		adr:      "0005",
		why: "Same curve and same argument as the stall window; ADR 0005 " +
			"considered a min(learned, constant) clamp here specifically and " +
			"rejected it, because it reintroduces false kills exactly where the " +
			"learned data says the constant is wrong.",
	},
}

// TestEffectiveThresholdRegistryIsExhaustive parses the package for every
// effective* method on CheckInstanceParams and requires each to be registered.
// Reflection cannot enumerate unexported methods, so the source is the only
// authority on what exists. The scan covers the whole package, not one file,
// because splitting a method into a new file would otherwise carry it out of
// scope without failing anything.
func TestEffectiveThresholdRegistryIsExhaustive(t *testing.T) {
	fset := token.NewFileSet()
	nonTest := func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }
	pkgs, err := parser.ParseDir(fset, ".", nonTest, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	declared := map[string]bool{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || !strings.HasPrefix(fn.Name.Name, "effective") {
					continue
				}
				if receiverTypeName(fn.Recv.List[0].Type) != "CheckInstanceParams" {
					continue
				}
				declared[fn.Name.Name] = true
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no effective* methods on CheckInstanceParams — the parser or the naming convention changed, and this guard is now inert")
	}

	registered := map[string]bool{}
	for _, p := range thresholdPolicies {
		registered[p.method] = true
	}

	for name := range declared {
		if !registered[name] {
			t.Errorf("%s is not in thresholdPolicies. A learned survival threshold can widen this window, and three incidents have come from one doing so unbounded. Add an entry with either a ceiling or an exemption naming why unbounded is safe, and record the decision in docs/decisions/.", name)
		}
	}
	for name := range registered {
		if !declared[name] {
			t.Errorf("thresholdPolicies lists %s, which no longer exists — drop the entry", name)
		}
	}
}

// receiverTypeName returns the base type name of a method receiver, so a
// pointer receiver cannot slip past the scan as an unrecognized expression.
func receiverTypeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// TestEffectiveThresholdRegistryIsWellFormed: an exemption is only meaningful
// if it carries its reasoning, a ceiling is only meaningful if it bounds
// something, and an ADR citation is only meaningful if the record exists.
func TestEffectiveThresholdRegistryIsWellFormed(t *testing.T) {
	for _, p := range thresholdPolicies {
		if p.learned == nil {
			t.Errorf("%s: no curve constructor — the clamp test cannot drive it", p.method)
		}
		if p.fallback <= 0 {
			t.Errorf("%s: no fallback constant recorded", p.method)
		}
		switch {
		case p.exempt && p.ceiling != 0:
			t.Errorf("%s: exempt entries must not set a ceiling", p.method)
		case p.exempt && strings.TrimSpace(p.why) == "":
			t.Errorf("%s: exempt without a reason — say why an unbounded learned value is safe here", p.method)
		case !p.exempt && p.ceiling <= 0:
			t.Errorf("%s: clamped entries need a positive ceiling", p.method)
		}
		if p.adr == "" {
			t.Errorf("%s: no ADR recorded", p.method)
			continue
		}
		matches, err := filepath.Glob(filepath.Join("..", "..", "docs", "decisions", p.adr+"-*.md"))
		if err != nil {
			t.Fatalf("glob docs/decisions: %v", err)
		}
		if len(matches) == 0 {
			t.Errorf("%s: cites ADR %s, which does not exist in docs/decisions/", p.method, p.adr)
		}
	}
}

// TestEffectiveThresholdClampsHold drives each method with a learned value far
// past any plausible window and checks the registry's claim. A clamped entry
// must return exactly its ceiling and an exempt entry exactly the learned
// value, so both are positive statements: asserting only "at most the ceiling"
// would also pass for a method that dropped its learned branch and returned
// the much smaller fallback constant, which is the regression ADR 0010
// quantifies.
func TestEffectiveThresholdClampsHold(t *testing.T) {
	const absurd = 6 * time.Hour
	for _, p := range thresholdPolicies {
		t.Run(p.method, func(t *testing.T) {
			got := p.eval(p.learned(absurd))
			if p.exempt {
				if got != absurd {
					t.Errorf("%s = %s, want the learned %s passed through (registry marks it exempt per ADR %s)", p.method, got, absurd, p.adr)
				}
				return
			}
			if got != p.ceiling {
				t.Errorf("%s = %s, want the ceiling %s (ADR %s)", p.method, got, p.ceiling, p.adr)
			}
		})
	}
}

// TestEffectiveThresholdFallbacksAreUsedWhenUnlearned: with no learned data
// the constant must survive. A clamp applied to the fallback path would
// silently retune every threshold on a fresh database.
func TestEffectiveThresholdFallbacksAreUsedWhenUnlearned(t *testing.T) {
	for _, p := range thresholdPolicies {
		t.Run(p.method, func(t *testing.T) {
			if got := p.eval(CheckInstanceParams{}); got != p.fallback {
				t.Errorf("%s with no learned data = %s, want the constant %s", p.method, got, p.fallback)
			}
		})
	}
}
