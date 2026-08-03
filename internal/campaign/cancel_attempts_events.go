package campaign

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/db"
)

// RecordCancelAttemptsDeliveryFailure makes a failed cancel-attempts control
// write visible through lifecycle-event diagnostics.
func RecordCancelAttemptsDeliveryFailure(database *sql.DB, launchID int64, attemptIDs []int64, detail string, deliveryErr error) error {
	if database == nil || deliveryErr == nil {
		return nil
	}
	return db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventGraceCancelAttemptsFailed,
		LaunchID:  launchID,
		JobCount:  len(attemptIDs),
		Detail:    formatCancelAttemptsFailureDetail(attemptIDs, detail),
		ErrorText: deliveryErr.Error(),
	})
}

func formatCancelAttemptsFailureDetail(attemptIDs []int64, detail string) string {
	var b strings.Builder
	b.WriteString("attempt_ids=")
	b.WriteString(fmt.Sprint(attemptIDs))
	if detail != "" {
		b.WriteString(" ")
		b.WriteString(detail)
	}
	return b.String()
}
