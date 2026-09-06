package db

import (
	"fmt"
	"sync/atomic"
)

// localLedgerRefusal holds the invoked command path when this process must not
// open a local ledger at all. It is set on hosts whose role is "edge": the one
// job database lives on the hub, and a command that reaches for a local ledger
// on an edge has bypassed the edge-mode command gate — it must fail loudly
// rather than manufacture a second, empty database and report it as truth.
var localLedgerRefusal atomic.Value // string; absent or "" means opens are allowed

// RefuseLocalLedger makes every ledger open in this process fail before any
// directory or file is created. command is the invoked command path, used in
// the error text; "weft" when unknown. The returned function restores the
// previous state; Execute ignores it, tests use it for cleanup.
func RefuseLocalLedger(command string) func() {
	previous, _ := localLedgerRefusal.Load().(string)
	if command == "" {
		command = "weft"
	}
	localLedgerRefusal.Store(command)
	return func() {
		if previous == "" {
			localLedgerRefusal.Store("")
		} else {
			localLedgerRefusal.Store(previous)
		}
	}
}

// localLedgerRefusedError returns the refusal error when local ledgers are
// closed in this process, or nil when opens are allowed. kind is "job" or
// "bug" and selects the database named in the message.
func localLedgerRefusedError(kind string) error {
	command, _ := localLedgerRefusal.Load().(string)
	if command == "" {
		return nil
	}
	return fmt.Errorf("edge role: no local %s database; %s was not routed through the hub view",
		kind, command)
}
