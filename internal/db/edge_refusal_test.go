package db

import (
	"os"
	"testing"
)

func TestOpenRefusedOnEdge(t *testing.T) {
	restore := RefuseLocalLedger("jobs list")
	defer restore()

	for name, open := range map[string]func() (any, error){
		"Open":           func() (any, error) { return Open() },
		"OpenForReading": func() (any, error) { return OpenForReading() },
		"OpenReadOnly":   func() (any, error) { return OpenReadOnly() },
	} {
		if _, err := open(); err == nil {
			t.Errorf("%s: expected refusal error, got nil", name)
		} else {
			want := "edge role: no local job database; jobs list was not routed through the hub view"
			if err.Error() != want {
				t.Errorf("%s: error = %q, want %q", name, err.Error(), want)
			}
		}
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("refused opens must not create the database file; stat(%s) err = %v", dbPath, err)
	}
	if _, err := os.Stat(bugDBPath); !os.IsNotExist(err) {
		t.Errorf("refused opens must not create the bug database file; stat(%s) err = %v", bugDBPath, err)
	}
}

func TestOpenBugDBRefusedOnEdge(t *testing.T) {
	restore := RefuseLocalLedger("bug list")
	defer restore()

	if _, err := OpenBugDB(); err == nil {
		t.Fatal("OpenBugDB: expected refusal error, got nil")
	} else {
		want := "edge role: no local bug database; bug list was not routed through the hub view"
		if err.Error() != want {
			t.Errorf("OpenBugDB: error = %q, want %q", err.Error(), want)
		}
	}
	if _, err := os.Stat(bugDBPath); !os.IsNotExist(err) {
		t.Errorf("refused open must not create the bug database file; stat(%s) err = %v", bugDBPath, err)
	}
}

func TestOpenUnrefusedByDefault(t *testing.T) {
	database, err := Open()
	if err != nil {
		t.Fatalf("Open without refusal: %v", err)
	}
	database.Close()
}
