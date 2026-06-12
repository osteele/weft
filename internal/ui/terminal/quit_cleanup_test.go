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

	_, cmd, handled := handleListKeyBinding(model, "q", listCommonKeyBindings(false))
	if !handled {
		t.Fatal("expected q to be handled")
	}
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
