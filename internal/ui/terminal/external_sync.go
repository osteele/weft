package terminal

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/osteele/weft/internal/skypilot"
)

var skyClientForTUI = skypilot.Client{}

func syncExternalStateForTUI(ctx context.Context, database *sql.DB) []string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, _, err := skyClientForTUI.SyncBindingsAmbient(ctx, database, "")
	if err != nil {
		return []string{fmt.Sprintf("SkyPilot sync skipped: %v", err)}
	}
	return nil
}

func syncNonHostStateForTUI(ctx context.Context, database *sql.DB, full bool) []string {
	warnings := syncCloudStateForTUICtx(ctx, database, full)
	warnings = append(warnings, syncExternalStateForTUI(ctx, database)...)
	return compactWarnings(warnings)
}
