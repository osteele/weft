package db

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	coveringAttemptIndex  = "idx_job_attempts_authoritative_covering"
	untombstonedJobsIndex = "idx_jobs_untombstoned"
)

// TestJobStatusAttemptResolutionUsesCoveringIndex is the plan-shape guard for
// migration 00050: job_status is a view, so the two hot scans in its
// definition — the jobs scan for untombstoned rows and the per-row
// latest-authoritative-attempt subquery — must be served by the migration's
// indexes. SQLite emits the plan structurally, so no data is needed.
//
// Dropping either index from the migration flips the corresponding plan line
// back to a data-page scan and fails this test.
func TestJobStatusAttemptResolutionUsesCoveringIndex(t *testing.T) {
	database := SetupTestDB(t)

	query, args := listJobsByStatusesQuery([]string{StatusRunning, StatusStarting, StatusPaused}, "", "", 0, nil, "")
	plan := explainQueryPlan(t, database, query, args...)

	for _, want := range []string{
		"USING COVERING INDEX " + coveringAttemptIndex,
		untombstonedJobsIndex,
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("EXPLAIN QUERY PLAN does not mention %q:\n%s", want, plan)
		}
	}
}

// TestJobStatusIdleQueryIndexes is the wb79 measurement harness. It builds a
// synthetic database (~5k jobs, ~15k attempts, some open move intents) through
// the real schema, then times ListJobsByStatuses with an active-status filter
// and captures EXPLAIN QUERY PLAN before and after migration 00050's indexes.
//
// Timings are logged, never asserted: wall time is CI-hostile. The assertible
// part is the after-state plan, which must use the covering attempt index and
// the partial untombstoned-jobs index.
func TestJobStatusIdleQueryIndexes(t *testing.T) {
	database := SetupTestDB(t)

	dropIndexIfExists(t, database, coveringAttemptIndex)
	dropIndexIfExists(t, database, untombstonedJobsIndex)

	populateJobStatusSynthetic(t, database, 5000)

	statuses := []string{StatusRunning, StatusStarting, StatusPaused}
	query, args := listJobsByStatusesQuery(statuses, "", "", 0, nil, "")
	countQuery := `SELECT count(*) FROM job_status WHERE tombstoned = 0`

	t.Log("=== EXPLAIN QUERY PLAN (before: no wb79 indexes) ===")
	beforePlan := explainQueryPlan(t, database, query, args...)
	t.Log(beforePlan)
	t.Log("=== EXPLAIN QUERY PLAN count(*) (before) ===")
	t.Log(explainQueryPlan(t, database, countQuery))
	before := timeListJobsByStatuses(t, database, statuses)
	beforeCount := timeViewCount(t, database, countQuery)

	buildStart := time.Now()
	createIndex(t, database, coveringAttemptIndex,
		`CREATE INDEX IF NOT EXISTS `+coveringAttemptIndex+` ON job_attempts(job_id, attempt_number DESC, id DESC, abandoned_at, move_intent_id)`)
	createIndex(t, database, untombstonedJobsIndex,
		`CREATE INDEX IF NOT EXISTS `+untombstonedJobsIndex+` ON jobs(id) WHERE tombstoned = 0`)
	buildElapsed := time.Since(buildStart)
	t.Logf("index build: %s", buildElapsed)

	t.Log("=== EXPLAIN QUERY PLAN (after: wb79 indexes) ===")
	afterPlan := explainQueryPlan(t, database, query, args...)
	t.Log(afterPlan)
	t.Log("=== EXPLAIN QUERY PLAN count(*) (after) ===")
	t.Log(explainQueryPlan(t, database, countQuery))
	after := timeListJobsByStatuses(t, database, statuses)
	afterCount := timeViewCount(t, database, countQuery)

	t.Logf("ListJobsByStatuses(%v): before=%s after=%s (index build %s)",
		statuses, before, after, buildElapsed)
	t.Logf("count(*) WHERE tombstoned = 0: before=%s after=%s",
		beforeCount, afterCount)

	for _, want := range []string{
		"USING COVERING INDEX " + coveringAttemptIndex,
		untombstonedJobsIndex,
	} {
		if !strings.Contains(afterPlan, want) {
			t.Errorf("after-state EXPLAIN QUERY PLAN does not mention %q:\n%s", want, afterPlan)
		}
	}
}

// listJobsByStatusesQuery mirrors the statement ListJobsByStatuses builds, so
// EXPLAIN and timing measure the same query the function runs. Kept as a
// separate helper (rather than extracted into production code) because wb79
// forbids rewriting ListJobsByStatuses itself.
func listJobsByStatusesQuery(statuses []string, host, project string, limit int, tags []string, processedFilter string) (string, []interface{}) {
	query := fmt.Sprintf(`SELECT %s FROM job_status WHERE tombstoned = 0`, qualifiedJobSelectColumns("job_status"))
	args := make([]interface{}, 0, len(statuses)+4)

	if len(statuses) > 0 {
		placeholders := make([]string, 0, len(statuses))
		for _, status := range statuses {
			trimmed := strings.TrimSpace(status)
			if trimmed == "" {
				continue
			}
			placeholders = append(placeholders, "?")
			args = append(args, trimmed)
		}
		if len(placeholders) > 0 {
			query += ` AND status IN (` + strings.Join(placeholders, ", ") + `)`
		}
	}
	if host != "" {
		query += ` AND host = ?`
		args = append(args, host)
	}
	if project != "" {
		query += ` AND project = ?`
		args = append(args, project)
	}

	query += ` ORDER BY CASE WHEN status IN ('running', 'starting', 'paused') THEN 0 ELSE 1 END, id DESC`
	applyLimit := limit > 0 && len(normalizeTags(tags)) == 0 && processedFilter == ""
	if applyLimit {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	return query, args
}

func explainQueryPlan(t *testing.T, database *sql.DB, query string, args ...interface{}) string {
	t.Helper()
	rows, err := database.Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var id, parent int
		var notUsed, detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		fmt.Fprintf(&b, "  %d|%d|%s|%s\n", id, parent, notUsed, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan rows: %v", err)
	}
	return b.String()
}

// timeListJobsByStatuses runs the query three times and logs each run plus the
// best. Best-of-N is the least CI-hostile single number: a slow run from
// unrelated load lands in the per-run logs, not in the reported figure. A
// warmup run first brings both before/after states to the same page-cache
// condition, so a cold-cache outlier does not masquerade as a regression.
func timeListJobsByStatuses(t *testing.T, database *sql.DB, statuses []string) time.Duration {
	t.Helper()
	if _, err := ListJobsByStatuses(database, statuses, "", "", 0, nil, ""); err != nil {
		t.Fatalf("warmup ListJobsByStatuses: %v", err)
	}
	var best time.Duration
	for i := 0; i < 3; i++ {
		start := time.Now()
		jobs, err := ListJobsByStatuses(database, statuses, "", "", 0, nil, "")
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("ListJobsByStatuses: %v", err)
		}
		t.Logf("  run %d: %s (%d rows)", i+1, elapsed, len(jobs))
		if i == 0 || elapsed < best {
			best = elapsed
		}
	}
	t.Logf("  best of 3: %s", best)
	return best
}

// timeViewCount times the view's untombstoned count — the production headline
// measurement (1.25s on the 1.3 GB DB) — three times and logs each run plus
// the best, with a warmup run to normalize page cache.
func timeViewCount(t *testing.T, database *sql.DB, query string) time.Duration {
	t.Helper()
	var warmup int
	if err := database.QueryRow(query).Scan(&warmup); err != nil {
		t.Fatalf("warmup count query: %v", err)
	}
	var best time.Duration
	for i := 0; i < 3; i++ {
		start := time.Now()
		var count int
		if err := database.QueryRow(query).Scan(&count); err != nil {
			t.Fatalf("count query: %v", err)
		}
		elapsed := time.Since(start)
		t.Logf("  count run %d: %s (%d rows)", i+1, elapsed, count)
		if i == 0 || elapsed < best {
			best = elapsed
		}
	}
	t.Logf("  count best of 3: %s", best)
	return best
}

func createIndex(t *testing.T, database *sql.DB, name, ddl string) {
	t.Helper()
	if _, err := database.Exec(ddl); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
}

func dropIndexIfExists(t *testing.T, database *sql.DB, name string) {
	t.Helper()
	if _, err := database.Exec(`DROP INDEX IF EXISTS ` + name); err != nil {
		t.Fatalf("drop %s: %v", name, err)
	}
}

// populateJobStatusSynthetic fills a migrated database with n jobs, three
// attempts each (~3n attempts), a mix of terminal/active latest attempts, some
// abandoned attempts, some open move intents shadowing the latest attempt, and
// a ~10% tombstoned tail so the partial untombstoned-jobs index has rows to
// skip. All rows satisfy the schema's FK/CHECK constraints and triggers.
func populateJobStatusSynthetic(t *testing.T, database *sql.DB, n int) {
	t.Helper()
	now := time.Now().Unix()
	tx, err := database.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	jobs, err := tx.Prepare(`INSERT INTO jobs(id, working_dir, command, created_at, tombstoned) VALUES (?, '/tmp', 'echo', ?, ?)`)
	if err != nil {
		t.Fatalf("prepare jobs insert: %v", err)
	}
	defer jobs.Close()
	attempts, err := tx.Prepare(`INSERT INTO job_attempts(id, job_id, attempt_number, host, status, queued_at, start_time, end_time, exit_code, abandoned_at) VALUES (?, ?, ?, '', ?, ?, ?, ?, ?, NULL)`)
	if err != nil {
		t.Fatalf("prepare attempts insert: %v", err)
	}
	defer attempts.Close()
	moveIntents, err := tx.Prepare(`INSERT INTO move_intents(id, job_id, source_attempt_id, target_kind, state, created_at) VALUES (?, ?, ?, 'existing', 'open', ?)`)
	if err != nil {
		t.Fatalf("prepare move intent insert: %v", err)
	}
	defer moveIntents.Close()

	attemptID := 1
	moveID := 1
	for job := 1; job <= n; job++ {
		tombstoned := 0
		if job > n-n/10 {
			tombstoned = 1
		}
		if _, err := jobs.Exec(job, now, tombstoned); err != nil {
			t.Fatalf("insert job %d: %v", job, err)
		}
		for k := 1; k <= 3; k++ {
			var status string
			var end, exit interface{}
			switch k {
			case 1, 2:
				status = "failed"
				end, exit = now, 1
			case 3:
				switch job % 10 {
				case 0, 1, 2:
					status = "running"
				case 3, 4, 5, 6:
					status = "completed"
					end, exit = now, 0
				default:
					status = "failed"
					end, exit = now, 1
				}
			}
			if _, err := attempts.Exec(attemptID, job, k, status, now, now-10, end, exit); err != nil {
				t.Fatalf("insert attempt %d: %v", attemptID, err)
			}
			if k == 3 && job%5 == 0 {
				if _, err := tx.Exec(`UPDATE job_attempts SET abandoned_at = ? WHERE id = ?`, now, attemptID); err != nil {
					t.Fatalf("abandon attempt %d: %v", attemptID, err)
				}
			}
			attemptID++
		}
		if job%7 == 0 {
			if _, err := moveIntents.Exec(moveID, job, attemptID-1, now); err != nil {
				t.Fatalf("insert move intent %d: %v", moveID, err)
			}
			moveID++
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
