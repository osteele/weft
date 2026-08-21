package terminal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/jobview"
)

// The launch spinner ticks at 10 Hz (spinner.Dot, FPS = time.Second/10), and
// every tick re-enters hasActiveLaunchingSpinner. Anything that reaches SQLite
// from that path is multiplied by ten per second for as long as a launch stays
// un-ready, so these tests assert query counts rather than wall-clock time: a
// timing assertion passes on a fast machine while the query count regresses.
//
// The fixture deliberately contains a launching-but-not-ready job. The tick
// loop stops rescheduling itself when nothing is launching (list_tui.go
// "case spinner.TickMsg"), so a genuinely idle model never ticks and any
// budget measured against one passes trivially without exercising this path.

// countingDriver wraps a driver and counts queries issued through it, on both
// the QueryerContext fast path and the Prepare/Stmt fallback.
type countingDriver struct {
	inner driver.Driver
	count *atomic.Int64
}

func (d countingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn, count: d.count}, nil
}

type countingConn struct {
	driver.Conn
	count *atomic.Int64
}

func (c *countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		// Fall back to the Prepare path, which counts in countingStmt.
		return nil, driver.ErrSkip
	}
	c.count.Add(1)
	return queryer.QueryContext(ctx, query, args)
}

func (c *countingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	var (
		stmt driver.Stmt
		err  error
	)
	if preparer, ok := c.Conn.(driver.ConnPrepareContext); ok {
		stmt, err = preparer.PrepareContext(ctx, query)
	} else {
		stmt, err = c.Conn.Prepare(query)
	}
	if err != nil {
		return nil, err
	}
	return &countingStmt{Stmt: stmt, count: c.count}, nil
}

type countingStmt struct {
	driver.Stmt
	count *atomic.Int64
}

func (s *countingStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := s.Stmt.(driver.StmtQueryContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	s.count.Add(1)
	return queryer.QueryContext(ctx, args)
}

// openCountingDB opens a second handle to the test database through a
// query-counting driver. Seeding goes through the caller's ordinary handle so
// only the measured call contributes to the count.
func openCountingDB(t *testing.T, path string) (*sql.DB, *atomic.Int64) {
	t.Helper()

	count := &atomic.Int64{}
	base, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatalf("open base driver handle: %v", err)
	}
	inner := base.Driver()
	if err := base.Close(); err != nil {
		t.Fatalf("close base driver handle: %v", err)
	}

	// A driver name may only be registered once per process, so each call
	// registers a fresh name and the counter travels with the connection.
	name := fmt.Sprintf("sqlite-counting-%d", countingDriverSeq.Add(1))
	sql.Register(name, countingDriver{inner: inner, count: count})

	connStr := fmt.Sprintf("file:%s?_pragma=foreign_keys(ON)", path)
	database, err := sql.Open(name, connStr)
	if err != nil {
		t.Fatalf("open counting database: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database, count
}

var countingDriverSeq atomic.Int64

// launchingSpinnerModel builds a status-grouped model holding jobCount queued
// jobs, the last of which displays as launching-but-not-ready. Placing the
// launching job last makes the scan traverse every preceding job, which is the
// shape that turned a per-job call into a per-job query.
func launchingSpinnerModel(t *testing.T, jobCount int) (listTUIModel, *atomic.Int64) {
	t.Helper()

	seed := db.SetupTestDB(t)
	if _, err := db.CreateLaunch(seed, &db.Launch{
		Status:           db.LaunchStatusRunning,
		CostPerHourCents: 45,
	}); err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	jobs := make([]*db.Job, 0, jobCount)
	placementStatus := make(map[int64]jobview.PlacementStatus, jobCount)
	for i := 0; i < jobCount; i++ {
		jobID, err := db.RecordQueued(seed, "", t.TempDir(), "python train.py", "queued")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		job, err := db.GetJobByID(seed, jobID)
		if err != nil {
			t.Fatalf("GetJobByID: %v", err)
		}
		jobs = append(jobs, job)
		bucket := jobview.BucketQueued
		if i == jobCount-1 {
			bucket = jobview.BucketLaunching
		}
		placementStatus[jobID] = jobview.PlacementStatus{Bucket: bucket}
	}

	counting, count := openCountingDB(t, db.Path())
	m := listTUIModel{
		database:               counting,
		groupedByStatus:        true,
		jobs:                   jobs,
		placementStatusByJob:   placementStatus,
		autoRunRateTargetCents: 350,
	}
	return m, count
}

// TestHasActiveLaunchingSpinnerQueriesDoNotScaleWithJobCount is the regression
// guard: hasActiveLaunchingSpinner once evaluated launchesWithActiveJob (and
// so RunRateHeadroom, and so a SUM over launches) once per job it scanned.
func TestHasActiveLaunchingSpinnerQueriesDoNotScaleWithJobCount(t *testing.T) {
	small, smallCount := launchingSpinnerModel(t, 4)
	large, largeCount := launchingSpinnerModel(t, 40)

	if !small.hasActiveLaunchingSpinner() {
		t.Fatal("small fixture: want an active launching spinner, got none")
	}
	if !large.hasActiveLaunchingSpinner() {
		t.Fatal("large fixture: want an active launching spinner, got none")
	}

	got, want := largeCount.Load(), smallCount.Load()
	if got != want {
		t.Fatalf("query count scales with job count: 4 jobs issued %d queries, 40 jobs issued %d; want equal", want, got)
	}
}

// TestHasActiveLaunchingSpinnerIssuesOneQueryPerCall pins the absolute cost.
// At 10 Hz this is the per-second SQLite budget for the spinner path.
func TestHasActiveLaunchingSpinnerIssuesOneQueryPerCall(t *testing.T) {
	m, count := launchingSpinnerModel(t, 12)

	if !m.hasActiveLaunchingSpinner() {
		t.Fatal("want an active launching spinner, got none")
	}

	if got := count.Load(); got != 1 {
		t.Fatalf("hasActiveLaunchingSpinner issued %d queries, want 1", got)
	}
}
