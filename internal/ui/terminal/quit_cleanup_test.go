package terminal

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/app/hostsync"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

func TestListTUIQuitSchedulesSyncWorkerCleanup(t *testing.T) {
	database := db.SetupTestDB(t)
	worker := hostsync.New(database, nil, nil, &config.Config{})
	ctx, cancel := context.WithCancel(context.Background())

	cleanupCalls := 0
	oldRunCleanup := runTUIQuitCleanup
	runTUIQuitCleanup = func(func()) {
		cleanupCalls++
	}
	defer func() { runTUIQuitCleanup = oldRunCleanup }()

	model := listTUIModel{
		database:   database,
		ctx:        ctx,
		cancel:     cancel,
		syncWorker: worker,
	}

	next, cmd, handled := handleListKeyBinding(model, "q", listCommonKeyBindings(false))
	if !handled {
		t.Fatal("expected q to be handled")
	}
	// Quit cancels background work and schedules the sync-worker cleanup right
	// away, but defers the (potentially slow) auto-lease release to a command so
	// it never blocks the update goroutine.
	if cleanupCalls != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanupCalls)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("expected quit to cancel context immediately")
	}
	lm, ok := next.(listTUIModel)
	if !ok {
		t.Fatalf("expected listTUIModel, got %T", next)
	}
	if !lm.quitting {
		t.Fatal("expected quit to enter the quitting state")
	}
	if cmd == nil {
		t.Fatal("expected a teardown command")
	}
	// The teardown resolves to listQuitNowMsg, which the model turns into quit.
	_, quitCmd := lm.Update(listQuitNowMsg{})
	assertQuitCmd(t, quitCmd)
}

func TestHostsTUIQuitSchedulesSyncWorkerCleanup(t *testing.T) {
	database := db.SetupTestDB(t)
	worker := hostsync.New(database, nil, nil, &config.Config{})
	ctx, cancel := context.WithCancel(context.Background())

	cleanupCalls := 0
	oldRunCleanup := runTUIQuitCleanup
	runTUIQuitCleanup = func(func()) {
		cleanupCalls++
	}
	defer func() { runTUIQuitCleanup = oldRunCleanup }()

	model := hostsTUIModel{
		database:   database,
		ctx:        ctx,
		cancel:     cancel,
		syncWorker: worker,
	}

	_, cmd := model.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	assertQuitCmd(t, cmd)
	if cleanupCalls != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanupCalls)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("expected quit to cancel context immediately")
	}
}
