package cmd

import (
	"strings"

	"github.com/spf13/cobra"
)

const (
	jobIDPrefix      = "wj"
	instanceIDPrefix = "wi"
)

type idTargetKind int

const (
	idTargetUnknown idTargetKind = iota
	idTargetJob
	idTargetInstance
)

// idPrefixesSeen reports which explicit ID prefixes (wj/wi) appear across the
// arguments. Bare numerics set neither flag — they carry no entity hint, and
// since instance IDs are always wi-prefixed a bare number is only ever a job.
func idPrefixesSeen(args []string) (sawJob, sawInstance bool) {
	for _, arg := range args {
		for _, token := range splitIDPrefixScanTokens(arg) {
			token = strings.ToLower(strings.TrimSpace(token))
			switch {
			case strings.HasPrefix(token, jobIDPrefix):
				sawJob = true
			case strings.HasPrefix(token, instanceIDPrefix):
				sawInstance = true
			}
		}
	}
	return sawJob, sawInstance
}

// resolveIDTargetKind infers whether args refer to jobs or instances.
// It requires at least one explicit prefix (wj/wi) across all arguments, so
// bare numerics are reported as ambiguous. Use this only for commands that
// genuinely dispatch to both entity types (e.g. the destructive terminate
// router); job-only commands should use ParseJobIDsForJobCommand instead.
func resolveIDTargetKind(args []string) (idTargetKind, error) {
	sawJob, sawInstance := idPrefixesSeen(args)

	if sawJob && sawInstance {
		return idTargetUnknown, usageErrorf("cannot mix job and instance IDs in one command; use only wj... or only wi...")
	}
	if sawJob {
		return idTargetJob, nil
	}
	if sawInstance {
		return idTargetInstance, nil
	}
	return idTargetUnknown, usageErrorf("ambiguous ID(s): use wj... for jobs or wi... for instances")
}

func splitIDPrefixScanTokens(arg string) []string {
	// Split around separators used by ID lists/ranges.
	return strings.FieldsFunc(arg, func(r rune) bool {
		switch r {
		case ',', ':', '.':
			return true
		default:
			return false
		}
	})
}

var runJobTerminateFunc = runKill
var runInstanceTerminateFunc = runInstanceTerminate

func runTerminate(cmd *cobra.Command, args []string) error {
	kind, err := resolveIDTargetKind(args)
	if err != nil {
		return err
	}
	if kind == idTargetInstance {
		return runInstanceTerminateFunc(cmd, args)
	}
	return runJobTerminateFunc(cmd, args)
}
