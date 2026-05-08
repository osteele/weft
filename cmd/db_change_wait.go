package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/osteele/weft/internal/app/dbwatch"
)

func openDBChangeSource(label string) *dbwatch.Source {
	source, err := dbwatch.OpenChangeSource()
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft %s: DB watch unavailable: %v\n", label, err)
		return nil
	}
	return source
}

func waitForDBChangeOrTimeout(ctx context.Context, source *dbwatch.Source, maxWait time.Duration, label string) (changed bool, done bool) {
	if source == nil {
		return false, waitForDurationOrDone(ctx, maxWait)
	}
	changed, err := source.Wait(ctx, maxWait)
	if ctx.Err() != nil {
		return false, true
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft %s: DB watch error: %v\n", label, err)
		return false, waitForDurationOrDone(ctx, maxWait)
	}
	return changed, false
}

func waitForDurationOrDone(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		<-ctx.Done()
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}
