package ops

import (
	"database/sql"

	"github.com/osteele/weft/internal/db"
)

func recordQueueDispatchOK(database *sql.DB, jobID int64) {
	_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventQueueDispatchOK,
		JobID:     jobID,
	})
}
