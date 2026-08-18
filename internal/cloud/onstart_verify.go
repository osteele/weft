package cloud

import (
	"strings"
	"time"
)

// OnStartVerification is a three-state verdict on whether the provider
// materialised weft's OnStart script into the container filesystem. It is the
// pull-based channel the dud watchdog lacks: it reads the container rather
// than waiting on what the container pushes to R2. See ADR 0009.
type OnStartVerification int

const (
	// OnStartVerificationUnknown means the check could not be completed:
	// no SSH details, unreachable host, timeout, or an unreadable file.
	// Never sufficient for a destructive action — an unreachable container
	// is not a container without a script.
	OnStartVerificationUnknown OnStartVerification = iota
	// OnStartConfirmedInstalled means the script was read and weft's
	// sentinel was found.
	OnStartConfirmedInstalled
	// OnStartConfirmedMissing means the script was read and the sentinel was
	// absent. Positive evidence that the provider never installed it.
	OnStartConfirmedMissing
)

func (v OnStartVerification) String() string {
	switch v {
	case OnStartConfirmedInstalled:
		return "installed"
	case OnStartConfirmedMissing:
		return "missing"
	default:
		return "unknown"
	}
}

// OnStartSentinel is a substring unique to weft's OnStart script: the shell
// function every stage write funnels through, so it cannot drop out of
// DefaultOnStartCmd without the stage markers going with it. Matching what
// weft sent, rather than a provider's own placeholder text, is deliberate —
// see ADR 0009.
const OnStartSentinel = "_weft_stage"

// onStartScriptPath is where vast.ai places the container's start script.
//
// The check is vast.ai-only because the premise "sentinel absent from this
// file ⇒ weft's script was not installed" is provider-specific. RunPod rejects
// OnStartCmd outright and delivers bootstrap through the template's
// dockerStartCmd, so a RunPod pod that happened to ship a base-image
// /root/onstart.sh would read as confirmed-missing while being perfectly
// healthy — and this verdict destroys instances with no window.
const onStartScriptPath = "/root/onstart.sh"

// onStartVerifyCmd reports whether the sentinel is present without shipping
// the file back. An unreadable or absent script prints UNREADABLE and exits
// 0, so a missing file is reported as unknown rather than as a command
// failure indistinguishable from an SSH problem.
const onStartVerifyCmd = `test -r ` + onStartScriptPath +
	` || { echo WEFT_ONSTART_UNREADABLE; exit 0; }; ` +
	`grep -q ` + OnStartSentinel + ` ` + onStartScriptPath +
	` && echo WEFT_ONSTART_INSTALLED || echo WEFT_ONSTART_MISSING`

// OnStartVerifyGrace is how long after the provider reports `running` weft
// waits before asking the container whether its OnStart script was installed.
// Providers write the start script before starting sshd, so an SSH-reachable
// container has already had its script decided; the grace is insurance
// against a provider that orders those the other way.
const OnStartVerifyGrace = 2 * time.Minute

// OnStartVerifyTimeout hard-bounds the SSH round trip. An instance that
// cannot answer within it stays unknown, which is the safe verdict, and the
// next pass asks again. It must stay well under the fast cloud-sync budget
// (syncorch.FastCloudTimeoutCLI, 10s) or a single unreachable instance in the
// dud window makes `weft list` report a degraded sync.
const OnStartVerifyTimeout = 5 * time.Second

// ProviderSupportsOnStartVerification reports whether the premise this check
// rests on — sentinel absent from onStartScriptPath implies weft's script was
// not installed — holds for a provider. Callers that hold only a provider
// name use this to skip the provider API round trip that building an
// *Instance would cost.
func ProviderSupportsOnStartVerification(provider Provider) bool {
	return provider == ProviderVastai
}

// SupportsOnStartVerification reports whether the verification premise holds
// for the instance's provider. Kept as a pure predicate so the gate is
// testable without reaching the network: a test that instead pointed a
// non-vast.ai instance at an unresolvable host would pass whether or not the
// gate existed.
func SupportsOnStartVerification(inst *Instance) bool {
	return inst != nil && ProviderSupportsOnStartVerification(inst.Provider)
}

// VerifyOnStartInstalled reports whether weft's OnStart script is present on
// the instance's filesystem. Any failure to look — and any provider whose
// bootstrap does not arrive as an OnStart script — yields
// OnStartVerificationUnknown.
func VerifyOnStartInstalled(inst *Instance, timeout time.Duration) OnStartVerification {
	if !SupportsOnStartVerification(inst) {
		return OnStartVerificationUnknown
	}
	out, err := RunOnInstanceOnce(inst, onStartVerifyCmd, timeout)
	if err != nil {
		return OnStartVerificationUnknown
	}
	return classifyOnStartVerifyOutput(out)
}

// classifyOnStartVerifyOutput maps the probe's stdout to a verdict. Ordering
// matters: MISSING is checked before INSTALLED so that shell noise carrying
// both tokens cannot be read as a healthy container.
func classifyOnStartVerifyOutput(out string) OnStartVerification {
	switch {
	case strings.Contains(out, "WEFT_ONSTART_MISSING"):
		return OnStartConfirmedMissing
	case strings.Contains(out, "WEFT_ONSTART_INSTALLED"):
		return OnStartConfirmedInstalled
	default:
		return OnStartVerificationUnknown
	}
}
