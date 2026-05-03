package main

import (
	"fmt"
	"os"
	"sync/atomic"

	"github.com/osteele/weft/internal/oplog"
)

var agentFatalFile atomic.Value

func setAgentFatalFile(path string) {
	agentFatalFile.Store(path)
}

func fatalAgentGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				detail := fmt.Sprintf("%s: %v", name, r)
				oplog.Log(oplog.OpAgentPanic, oplog.WithDetail(detail))
				fmt.Fprintf(os.Stderr, "agent background task panic: %s\n", detail)
				writeAgentFatal(detail)
				panic(r)
			}
		}()
		fn()
	}()
}

func writeAgentFatal(detail string) {
	v := agentFatalFile.Load()
	if v == nil {
		return
	}
	path, ok := v.(string)
	if !ok || path == "" {
		return
	}
	_ = os.WriteFile(path, []byte(detail+"\n"), 0o644)
}
