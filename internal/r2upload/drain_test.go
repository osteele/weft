package r2upload

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubProcess is a [Process] that emits canned stderr lines on a script and
// returns at a script-controlled time. Tests drive forward progress (and
// therefore the watchdog) by choosing how fast lines are emitted and what
// bytes they report.
type stubProcess struct {
	lines     []scriptedLine // emitted in order
	exitErr   error
	exitDelay time.Duration

	pr       *io.PipeReader
	pw       *io.PipeWriter
	done     chan struct{}
	killOnce sync.Once
	killed   chan struct{}
}

type scriptedLine struct {
	delay time.Duration // after Start (or after previous line)
	text  string
}

func newStubProcess(lines []scriptedLine, exitDelay time.Duration, exitErr error) *stubProcess {
	pr, pw := io.Pipe()
	return &stubProcess{
		lines:     lines,
		exitErr:   exitErr,
		exitDelay: exitDelay,
		pr:        pr,
		pw:        pw,
		done:      make(chan struct{}),
		killed:    make(chan struct{}),
	}
}

func (p *stubProcess) start() {
	go func() {
		defer close(p.done)
		for _, l := range p.lines {
			select {
			case <-time.After(l.delay):
			case <-p.killed:
				p.pw.Close()
				return
			}
			if _, err := io.WriteString(p.pw, l.text+"\n"); err != nil {
				return
			}
		}
		// hold the line — let the test's deadline drive exit if no
		// exit was scripted via exitDelay
		if p.exitDelay > 0 {
			select {
			case <-time.After(p.exitDelay):
			case <-p.killed:
			}
		} else {
			<-p.killed
		}
		p.pw.Close()
	}()
}

func (p *stubProcess) Stderr() io.Reader { return p.pr }

func (p *stubProcess) Wait() error {
	<-p.done
	if isClosed(p.killed) {
		return errors.New("killed")
	}
	return p.exitErr
}

func (p *stubProcess) Kill() {
	p.killOnce.Do(func() { close(p.killed) })
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// stubRunner returns a fixed stubProcess (one-shot).
type stubRunner struct {
	proc     *stubProcess
	startErr error
	called   bool
}

func (r *stubRunner) Start(_ context.Context, _ []string) (Process, error) {
	if r.startErr != nil {
		return nil, r.startErr
	}
	if r.called {
		return nil, errors.New("stubRunner already used")
	}
	r.called = true
	r.proc.start()
	return r.proc, nil
}

func statsLine(bytes string) string {
	return `{"level":"notice","msg":"Transferred: ` + bytes + ` / 100 MiB, 1%, 1 MiB/s, ETA 1m"}`
}

func TestDrainHappyPath(t *testing.T) {
	proc := newStubProcess([]scriptedLine{
		{delay: 5 * time.Millisecond, text: statsLine("1 MiB")},
		{delay: 5 * time.Millisecond, text: statsLine("10 MiB")},
		{delay: 5 * time.Millisecond, text: statsLine("100 MiB")},
	}, 5*time.Millisecond, nil)
	r := Drain(context.Background(), Options{
		Source: "/tmp/x", DestRemote: "r2:b/k", TotalBytes: 100 * 1024 * 1024,
		Runner:       &stubRunner{proc: proc},
		StallTimeout: 200 * time.Millisecond,
		TickInterval: 5 * time.Millisecond,
	})
	if r.Status != StatusOK {
		t.Fatalf("status = %q (reason=%q stderr=%q), want ok", r.Status, r.Reason, r.StderrTail)
	}
	if r.BytesUploaded < 100*1024*1024 {
		t.Errorf("BytesUploaded = %d, want >= 100MiB", r.BytesUploaded)
	}
}

func TestDrainStallKill(t *testing.T) {
	// One progress line then nothing — should trigger stall watchdog.
	proc := newStubProcess([]scriptedLine{
		{delay: 5 * time.Millisecond, text: statsLine("1 MiB")},
	}, 0, nil) // exitDelay=0 → process holds until killed
	r := Drain(context.Background(), Options{
		Source: "/tmp/x", DestRemote: "r2:b/k", TotalBytes: 1 << 30,
		Runner:       &stubRunner{proc: proc},
		StallTimeout: 80 * time.Millisecond,
		TickInterval: 10 * time.Millisecond,
		MaxDrain:     5 * time.Second,
	})
	if r.Status != StatusStalled {
		t.Fatalf("status = %q (reason=%q), want stalled", r.Status, r.Reason)
	}
	if r.KilledBy != KilledByStall {
		t.Errorf("KilledBy = %q, want %q", r.KilledBy, KilledByStall)
	}
	if r.BytesUploaded != 1024*1024 {
		t.Errorf("BytesUploaded = %d, want 1 MiB (the last observed)", r.BytesUploaded)
	}
	if !strings.Contains(r.Reason, "no progress") {
		t.Errorf("Reason = %q, want 'no progress' phrase", r.Reason)
	}
}

func TestDrainCeilingKill(t *testing.T) {
	// Keep bytes moving but slowly enough that the ceiling fires first.
	proc := newStubProcess([]scriptedLine{
		{delay: 10 * time.Millisecond, text: statsLine("1 MiB")},
		{delay: 10 * time.Millisecond, text: statsLine("2 MiB")},
		{delay: 10 * time.Millisecond, text: statsLine("3 MiB")},
		{delay: 10 * time.Millisecond, text: statsLine("4 MiB")},
		{delay: 10 * time.Millisecond, text: statsLine("5 MiB")},
		{delay: 10 * time.Millisecond, text: statsLine("6 MiB")},
		{delay: 10 * time.Millisecond, text: statsLine("7 MiB")},
		{delay: 10 * time.Millisecond, text: statsLine("8 MiB")},
		{delay: 10 * time.Millisecond, text: statsLine("9 MiB")},
		{delay: 10 * time.Millisecond, text: statsLine("10 MiB")},
	}, 0, nil)
	r := Drain(context.Background(), Options{
		Source: "/tmp/x", DestRemote: "r2:b/k", TotalBytes: 100 * 1024 * 1024,
		Runner: &stubRunner{proc: proc},
		// Force the ceiling well under the natural completion time.
		Baseline: 0, MaxDrain: 50 * time.Millisecond,
		FloorThroughput: 1, // 1 byte/sec → ceiling would be huge if uncapped
		StallTimeout:    5 * time.Second,
		TickInterval:    5 * time.Millisecond,
	})
	if r.Status != StatusCeiling {
		t.Fatalf("status = %q (reason=%q), want ceiling", r.Status, r.Reason)
	}
	if r.KilledBy != KilledByCeiling {
		t.Errorf("KilledBy = %q, want %q", r.KilledBy, KilledByCeiling)
	}
}

func TestDrainExitError(t *testing.T) {
	proc := newStubProcess([]scriptedLine{
		{delay: 5 * time.Millisecond, text: statsLine("1 MiB")},
	}, 5*time.Millisecond, errors.New("rclone broke"))
	r := Drain(context.Background(), Options{
		Source: "/tmp/x", DestRemote: "r2:b/k", TotalBytes: 1 << 20,
		Runner:       &stubRunner{proc: proc},
		StallTimeout: 5 * time.Second,
		TickInterval: 5 * time.Millisecond,
	})
	if r.Status != StatusError {
		t.Fatalf("status = %q (reason=%q), want error", r.Status, r.Reason)
	}
	if r.Err == nil {
		t.Error("Err is nil, want underlying rclone error")
	}
}

func TestDrainStartError(t *testing.T) {
	r := Drain(context.Background(), Options{
		Source: "/tmp/x", DestRemote: "r2:b/k",
		Runner: &stubRunner{startErr: errors.New("rclone not found")},
	})
	if r.Status != StatusError {
		t.Fatalf("status = %q, want error", r.Status)
	}
	if !strings.Contains(r.Reason, "rclone start failed") {
		t.Errorf("Reason = %q, want 'rclone start failed' phrase", r.Reason)
	}
}

func TestDrainCtxCancel(t *testing.T) {
	proc := newStubProcess([]scriptedLine{
		{delay: 5 * time.Millisecond, text: statsLine("1 MiB")},
	}, 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	r := Drain(ctx, Options{
		Source: "/tmp/x", DestRemote: "r2:b/k", TotalBytes: 1 << 30,
		Runner:       &stubRunner{proc: proc},
		StallTimeout: 5 * time.Second,
		TickInterval: 5 * time.Millisecond,
	})
	if r.Status != StatusError {
		t.Fatalf("status = %q, want error", r.Status)
	}
	if r.KilledBy != KilledByCanceled {
		t.Errorf("KilledBy = %q, want %q", r.KilledBy, KilledByCanceled)
	}
}

func TestComputeCeiling(t *testing.T) {
	cases := []struct {
		name     string
		opts     Options
		expected time.Duration
	}{
		{"size 0 → baseline only", Options{Baseline: 60 * time.Second, FloorThroughput: 256 * 1024, MaxDrain: 15 * time.Minute}, 60 * time.Second},
		{"10 MiB at 256 KiB/s + 60 s baseline = 100 s",
			Options{TotalBytes: 10 * 1024 * 1024, Baseline: 60 * time.Second, FloorThroughput: 256 * 1024, MaxDrain: 15 * time.Minute}, 100 * time.Second},
		{"5 GiB capped at MaxDrain",
			Options{TotalBytes: 5 * 1024 * 1024 * 1024, Baseline: 60 * time.Second, FloorThroughput: 256 * 1024, MaxDrain: 15 * time.Minute}, 15 * time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := computeCeiling(applyDefaults(c.opts))
			if got != c.expected {
				t.Errorf("got %s, want %s", got, c.expected)
			}
		})
	}
}
