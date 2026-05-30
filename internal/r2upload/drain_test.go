package r2upload

import (
	"context"
	"errors"
	"io"
	"strconv"
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
	// One progress line then nothing — should trigger mid-transfer stall.
	proc := newStubProcess([]scriptedLine{
		{delay: 5 * time.Millisecond, text: statsLine("1 MiB")},
	}, 0, nil) // exitDelay=0 → process holds until killed
	r := Drain(context.Background(), Options{
		Source: "/tmp/x", DestRemote: "r2:b/k", TotalBytes: 1 << 30,
		Runner:           &stubRunner{proc: proc},
		StallTimeout:     80 * time.Millisecond,
		HeartbeatTimeout: 5 * time.Second,
		TickInterval:     10 * time.Millisecond,
		MaxDrain:         5 * time.Second,
	})
	if r.Status != StatusStalled {
		t.Fatalf("status = %q (reason=%q), want stalled", r.Status, r.Reason)
	}
	if r.KilledBy != KilledByStall {
		t.Errorf("KilledBy = %q, want %q", r.KilledBy, KilledByStall)
	}
	if r.StallKind != StallKindMidTransfer {
		t.Errorf("StallKind = %q, want %q", r.StallKind, StallKindMidTransfer)
	}
	if r.BytesUploaded != 1024*1024 {
		t.Errorf("BytesUploaded = %d, want 1 MiB (the last observed)", r.BytesUploaded)
	}
	if !strings.Contains(r.Reason, "mid-transfer stall") {
		t.Errorf("Reason = %q, want 'mid-transfer stall' phrase", r.Reason)
	}
}

// TestDrainZeroByteStatsResetHeartbeat: when rclone emits "Transferred: 0 B / X"
// stats lines during pre-transfer setup, the heartbeat must keep resetting so
// the watchdog doesn't fire just because the byte counter is stuck at zero.
// This is the regression test for the bug where wi3333/wi3334 self-destructed
// after 30s with 0/1.5GiB uploaded — rclone was alive but had not started
// streaming bytes yet.
func TestDrainZeroByteStatsResetHeartbeat(t *testing.T) {
	// rclone emits 0-byte stats every 20ms for 200ms, then bytes start
	// flowing. The 50ms StallTimeout would fire if we were tracking only
	// byte progress, but the InitialStallTimeout is generous and the
	// heartbeat keeps resetting because every line counts as activity.
	lines := []scriptedLine{}
	for i := 0; i < 10; i++ {
		lines = append(lines, scriptedLine{delay: 20 * time.Millisecond, text: statsLine("0 B")})
	}
	lines = append(lines,
		scriptedLine{delay: 20 * time.Millisecond, text: statsLine("1 MiB")},
		scriptedLine{delay: 20 * time.Millisecond, text: statsLine("100 MiB")},
	)
	proc := newStubProcess(lines, 10*time.Millisecond, nil)
	r := Drain(context.Background(), Options{
		Source: "/tmp/x", DestRemote: "r2:b/k", TotalBytes: 100 * 1024 * 1024,
		Runner:              &stubRunner{proc: proc},
		StallTimeout:        100 * time.Millisecond, // tight, would mis-fire on byte-only tracking
		InitialStallTimeout: 2 * time.Second,        // generous pre-first-byte window
		HeartbeatTimeout:    500 * time.Millisecond, // slack for CI scheduling jitter
		TickInterval:        10 * time.Millisecond,
	})
	if r.Status != StatusOK {
		t.Fatalf("status = %q (reason=%q stderr=%q), want ok — 0-byte stats lines should keep watchdog alive",
			r.Status, r.Reason, r.StderrTail)
	}
}

// TestDrainHeartbeatStall: rclone is silent — no lines at all — and bytes
// never flow. Heartbeat fires first (it's shorter than InitialStallTimeout).
func TestDrainHeartbeatStall(t *testing.T) {
	// No scripted lines: rclone emits nothing.
	proc := newStubProcess(nil, 0, nil)
	r := Drain(context.Background(), Options{
		Source: "/tmp/x", DestRemote: "r2:b/k", TotalBytes: 1 << 30,
		Runner:              &stubRunner{proc: proc},
		StallTimeout:        5 * time.Second,
		InitialStallTimeout: 5 * time.Second,
		HeartbeatTimeout:    200 * time.Millisecond,
		TickInterval:        10 * time.Millisecond,
		MaxDrain:            5 * time.Second,
	})
	if r.Status != StatusStalled {
		t.Fatalf("status = %q (reason=%q), want stalled", r.Status, r.Reason)
	}
	if r.StallKind != StallKindHeartbeat {
		t.Errorf("StallKind = %q, want %q", r.StallKind, StallKindHeartbeat)
	}
	if !strings.Contains(r.Reason, "rclone silent") {
		t.Errorf("Reason = %q, want 'rclone silent' phrase", r.Reason)
	}
}

// TestDrainNeverStartedStall: rclone keeps emitting 0-byte stats (so heartbeat
// stays alive) but bytes never start flowing. InitialStallTimeout fires.
func TestDrainNeverStartedStall(t *testing.T) {
	lines := []scriptedLine{}
	for i := 0; i < 30; i++ {
		lines = append(lines, scriptedLine{delay: 10 * time.Millisecond, text: statsLine("0 B")})
	}
	proc := newStubProcess(lines, 0, nil)
	r := Drain(context.Background(), Options{
		Source: "/tmp/x", DestRemote: "r2:b/k", TotalBytes: 1 << 30,
		Runner:              &stubRunner{proc: proc},
		StallTimeout:        5 * time.Second,
		InitialStallTimeout: 200 * time.Millisecond,
		HeartbeatTimeout:    5 * time.Second,
		TickInterval:        10 * time.Millisecond,
		MaxDrain:            5 * time.Second,
	})
	if r.Status != StatusStalled {
		t.Fatalf("status = %q (reason=%q), want stalled", r.Status, r.Reason)
	}
	if r.StallKind != StallKindNeverStarted {
		t.Errorf("StallKind = %q, want %q", r.StallKind, StallKindNeverStarted)
	}
	if !strings.Contains(r.Reason, "no bytes uploaded") {
		t.Errorf("Reason = %q, want 'no bytes uploaded' phrase", r.Reason)
	}
	if r.BytesUploaded != 0 {
		t.Errorf("BytesUploaded = %d, want 0", r.BytesUploaded)
	}
}

// TestDrainSlowPaceKill: bytes flow steadily but at a rate far below the
// floor. After the pace-check warmup, abort with KilledBySlowPace so the
// instance doesn't burn $/hr waiting for a glacial upload.
func TestDrainSlowPaceKill(t *testing.T) {
	// 1 KiB every 20ms = 50 KiB/s. Floor 1 MiB/s × 0.25 fraction = 256 KiB/s
	// threshold → 50 KiB/s is well under and should fail.
	lines := []scriptedLine{}
	for i := 1; i <= 50; i++ {
		lines = append(lines, scriptedLine{
			delay: 20 * time.Millisecond,
			text:  kibLine(i),
		})
	}
	proc := newStubProcess(lines, 0, nil)
	r := Drain(context.Background(), Options{
		Source: "/tmp/x", DestRemote: "r2:b/k", TotalBytes: 10 * 1024 * 1024,
		Runner:                &stubRunner{proc: proc},
		StallTimeout:          5 * time.Second,
		InitialStallTimeout:   5 * time.Second,
		HeartbeatTimeout:      5 * time.Second,
		FloorThroughput:       1 << 20, // 1 MiB/s
		MinThroughputFraction: 0.25,    // require 256 KiB/s
		PaceCheckAfter:        150 * time.Millisecond,
		MaxDrain:              5 * time.Second,
		TickInterval:          10 * time.Millisecond,
	})
	if r.Status != StatusStalled {
		t.Fatalf("status = %q (reason=%q), want stalled", r.Status, r.Reason)
	}
	if r.KilledBy != KilledBySlowPace {
		t.Errorf("KilledBy = %q, want %q", r.KilledBy, KilledBySlowPace)
	}
	if r.StallKind != StallKindSlowPace {
		t.Errorf("StallKind = %q, want %q", r.StallKind, StallKindSlowPace)
	}
	if !strings.Contains(r.Reason, "sustained slow throughput") {
		t.Errorf("Reason = %q, want 'sustained slow throughput' phrase", r.Reason)
	}
	if r.ThroughputBytesPerSec <= 0 {
		t.Errorf("ThroughputBytesPerSec = %f, want > 0", r.ThroughputBytesPerSec)
	}
}

// TestDrainPaceCheckDisabled: with MinThroughputFraction < 0, a slow upload
// should NOT trigger the pace gate, even if it would otherwise.
func TestDrainPaceCheckDisabled(t *testing.T) {
	// Same scenario as TestDrainSlowPaceKill but with pace check disabled.
	// The ceiling will fire eventually, but pace check should not.
	lines := []scriptedLine{}
	for i := 1; i <= 5; i++ {
		lines = append(lines, scriptedLine{
			delay: 20 * time.Millisecond,
			text:  kibLine(i),
		})
	}
	proc := newStubProcess(lines, 5*time.Millisecond, nil)
	r := Drain(context.Background(), Options{
		Source: "/tmp/x", DestRemote: "r2:b/k", TotalBytes: 1 * 1024 * 1024,
		Runner:                &stubRunner{proc: proc},
		StallTimeout:          5 * time.Second,
		InitialStallTimeout:   5 * time.Second,
		HeartbeatTimeout:      5 * time.Second,
		FloorThroughput:       1 << 20,
		MinThroughputFraction: -1, // disabled
		PaceCheckAfter:        10 * time.Millisecond,
		MaxDrain:              5 * time.Second,
		TickInterval:          10 * time.Millisecond,
	})
	if r.Status != StatusOK {
		t.Fatalf("status = %q (reason=%q), want ok with pace check disabled", r.Status, r.Reason)
	}
}

// kibLine returns a stats line with N KiB transferred. Helper so tests can
// produce ascending byte counts that extractBytes parses as n * 1024.
func kibLine(n int) string {
	return statsLine(strconv.Itoa(n) + " KiB")
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
