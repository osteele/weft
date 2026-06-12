package terminal

import "github.com/osteele/weft/internal/app/hostsync"

var runTUIQuitCleanup = func(fn func()) {
	go fn()
}

func stopSyncWorkerAfterQuit(worker *hostsync.Worker) {
	if worker == nil {
		return
	}
	runTUIQuitCleanup(func() {
		worker.Stop()
	})
}
