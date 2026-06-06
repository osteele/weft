package cmd

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

const remoteWaitNoticeDelay = 1200 * time.Millisecond

func remoteLiveTargets(jobs []*db.Job) string {
	targets := map[string]bool{}
	for _, job := range jobs {
		if job == nil || db.IsTerminalStatus(job.Status) {
			continue
		}
		if job.HasInventoryHost() {
			targets[job.Host] = true
		} else if job.IsLaunchJob() {
			targets["rental instance"] = true
		}
	}
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func remoteWaitNotice(cmd *cobra.Command, format string, args ...any) func() {
	message := fmt.Sprintf(format, args...)
	return delayedNotice(cmd.ErrOrStderr(), remoteWaitNoticeDelay, message)
}

func delayedNotice(w io.Writer, delay time.Duration, message string) func() {
	if message == "" || !stderrIsTerminal() {
		return func() {}
	}
	done := make(chan struct{})
	timer := time.NewTimer(delay)
	go func() {
		defer timer.Stop()
		select {
		case <-timer.C:
			fmt.Fprintln(w, message)
		case <-done:
		}
	}()
	return func() {
		close(done)
	}
}

func stderrIsTerminal() bool {
	return term.IsTerminal(os.Stderr.Fd())
}
