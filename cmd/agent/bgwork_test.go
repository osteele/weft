package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/runner"
)

func TestBGWorkManager_RefCounts(t *testing.T) {
	jobs := []cloud.AgentJob{
		{ID: 1, Dir: "/tmp/test-project-a"},
		{ID: 2, Dir: "/tmp/test-project-a"},
		{ID: 3, Dir: "/tmp/test-project-b"},
	}
	m := newBGWorkManager(jobs, true) // skip deletion

	dirA := runner.ExpandTilde("/tmp/test-project-a")
	dirB := runner.ExpandTilde("/tmp/test-project-b")

	if m.workdirRefCount[dirA] != 2 {
		t.Errorf("refcount for dirA = %d, want 2", m.workdirRefCount[dirA])
	}
	if m.workdirRefCount[dirB] != 1 {
		t.Errorf("refcount for dirB = %d, want 1", m.workdirRefCount[dirB])
	}
}

func TestBGWorkManager_RegisterNewJobs(t *testing.T) {
	m := newBGWorkManager(nil, true)
	m.RegisterNewJobs([]cloud.AgentJob{
		{ID: 10, Dir: "/tmp/test-new"},
	})

	dir := runner.ExpandTilde("/tmp/test-new")
	if m.workdirRefCount[dir] != 1 {
		t.Errorf("refcount = %d, want 1", m.workdirRefCount[dir])
	}
}

func TestBGWorkManager_Barrier(t *testing.T) {
	m := newBGWorkManager(nil, true)

	var counter atomic.Int32
	for range 3 {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			time.Sleep(10 * time.Millisecond)
			counter.Add(1)
		}()
	}

	m.Barrier()
	if counter.Load() != 3 {
		t.Errorf("counter = %d, want 3", counter.Load())
	}
}

func TestBGWorkManager_BarrierLogsErrors(t *testing.T) {
	m := newBGWorkManager(nil, true)
	m.recordError(42, "test-op", errForTest("test error"))
	m.recordError(43, "test-op2", errForTest("another"))

	// Barrier should drain errors
	m.Barrier()

	m.mu.Lock()
	remaining := len(m.errors)
	m.mu.Unlock()
	if remaining != 0 {
		t.Errorf("errors after barrier = %d, want 0", remaining)
	}
}

type errForTest string

func (e errForTest) Error() string { return string(e) }
