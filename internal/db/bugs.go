package db

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const bugPrefix = "wb"

var legacyBugImportEnabled = true

type Bug struct {
	ID          int64
	Status      string
	Title       string
	Kind        string
	Scope       string
	Likelihood  string
	Severity    string
	Fingerprint string
	JobID       *int64
	Host        string
	Summary     string
	Detail      string
	Occurrences int
	CreatedAt   int64
	UpdatedAt   int64
	ClosedAt    *int64
	CloseReason string
}

type BugNote struct {
	ID        int64
	BugID     int64
	Body      string
	CreatedAt int64
}

type BugReport struct {
	Title       string
	Kind        string
	Scope       string
	Likelihood  string
	Severity    string
	Fingerprint string
	JobID       *int64
	Host        string
	Summary     string
	Detail      string
	Note        string
}

func FormatBugID(id int64) string {
	return fmt.Sprintf("%s%d", bugPrefix, id)
}

func ParseBugID(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(s, bugPrefix)
	if s == "" {
		return 0, fmt.Errorf("empty bug id")
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid bug id %q", s)
	}
	return id, nil
}

// BugPath returns the location of the standalone bug database.
func BugPath() string {
	return bugDBPath
}

func SetBugDBPath(path string) func() {
	original := bugDBPath
	bugDBPath = path
	return func() {
		bugDBPath = original
	}
}

// OpenBugDB opens the standalone bug database. It intentionally does not open
// or migrate the main jobs database, so bug reporting remains available when
// jobs.db has a schema mismatch or other unrelated failure.
func OpenBugDB() (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(bugDBPath), 0755); err != nil {
		return nil, fmt.Errorf("create bug database dir: %w", err)
	}
	connStr := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(30000)&_pragma=foreign_keys(ON)&_txlock=immediate", bugDBPath)
	database, err := sql.Open("sqlite", connStr)
	if err != nil {
		return nil, fmt.Errorf("open bug database: %w", err)
	}
	if err := initBugSchema(database); err != nil {
		database.Close()
		return nil, fmt.Errorf("init bug schema: %w", err)
	}
	if legacyBugImportEnabled {
		if err := importLegacyBugs(database); err != nil {
			slog.Debug("legacy bug import skipped", "error", err)
		}
	}
	return database, nil
}

func initBugSchema(database *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS bugs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			status TEXT NOT NULL DEFAULT 'open',
			title TEXT NOT NULL,
			kind TEXT NOT NULL DEFAULT 'bug',
			scope TEXT NOT NULL DEFAULT 'infrastructure',
			likelihood TEXT NOT NULL DEFAULT 'unknown',
			severity TEXT NOT NULL DEFAULT 'notice',
			fingerprint TEXT NOT NULL DEFAULT '',
			job_id INTEGER,
			host TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '',
			detail TEXT NOT NULL DEFAULT '',
			occurrences INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			closed_at INTEGER,
			close_reason TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_bugs_open_fingerprint
			ON bugs(fingerprint)
			WHERE status = 'open' AND fingerprint != ''`,
		`CREATE INDEX IF NOT EXISTS idx_bugs_status_updated
			ON bugs(status, updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS bug_notes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			bug_id INTEGER NOT NULL REFERENCES bugs(id) ON DELETE CASCADE,
			body TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_bug_notes_bug_created
			ON bug_notes(bug_id, created_at ASC, id ASC)`,
		`CREATE TABLE IF NOT EXISTS bug_imports (
			source TEXT NOT NULL,
			legacy_id INTEGER NOT NULL,
			bug_id INTEGER NOT NULL REFERENCES bugs(id) ON DELETE CASCADE,
			imported_at INTEGER NOT NULL,
			PRIMARY KEY (source, legacy_id)
		)`,
	}
	for _, stmt := range statements {
		if _, err := database.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func importLegacyBugs(database *sql.DB) error {
	if dbPath == "" || bugDBPath == "" || filepath.Clean(dbPath) == filepath.Clean(bugDBPath) {
		return nil
	}
	legacy, err := openLegacyBugDBReadOnly()
	if err != nil {
		return err
	}
	defer legacy.Close()
	if !legacyHasBugTables(legacy) {
		return nil
	}
	bugs, err := readLegacyBugs(legacy)
	if err != nil {
		return err
	}
	for _, bug := range bugs {
		if err := importLegacyBug(database, legacy, bug); err != nil {
			return err
		}
	}
	return nil
}

func openLegacyBugDBReadOnly() (*sql.DB, error) {
	connStr := fmt.Sprintf("file:%s?_pragma=busy_timeout(1000)&_pragma=foreign_keys(ON)&mode=ro", dbPath)
	database, err := sql.Open("sqlite", connStr)
	if err != nil {
		return nil, fmt.Errorf("open legacy jobs database: %w", err)
	}
	if err := database.Ping(); err != nil {
		database.Close()
		return nil, err
	}
	return database, nil
}

func legacyHasBugTables(database *sql.DB) bool {
	var count int
	err := database.QueryRow(`
		SELECT COUNT(*)
		  FROM sqlite_master
		 WHERE type = 'table'
		   AND name IN ('bugs', 'bug_notes')`).Scan(&count)
	return err == nil && count == 2
}

func readLegacyBugs(database *sql.DB) ([]Bug, error) {
	rows, err := database.Query(`
		SELECT id, status, title, kind, scope, likelihood, severity, fingerprint,
		       job_id, host, summary, detail, occurrences, created_at, updated_at,
		       closed_at, close_reason
		  FROM bugs ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBugs(rows)
}

func importLegacyBug(database, legacy *sql.DB, bug Bug) error {
	var imported int
	if err := database.QueryRow(`SELECT COUNT(*) FROM bug_imports WHERE source = 'jobs.db' AND legacy_id = ?`, bug.ID).Scan(&imported); err != nil {
		return err
	}
	if imported > 0 {
		return nil
	}

	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	targetID, err := findBugForLegacyImport(tx, bug)
	if err != nil {
		return err
	}
	if targetID == 0 {
		targetID, err = insertImportedBug(tx, bug)
		if err != nil {
			return err
		}
	} else if err := mergeImportedBug(tx, targetID, bug); err != nil {
		return err
	}

	if err := importLegacyBugNotes(tx, legacy, bug.ID, targetID); err != nil {
		return err
	}
	now := time.Now().Unix()
	legacyNote := fmt.Sprintf("Imported from legacy jobs.db bug %s.", FormatBugID(bug.ID))
	if _, err := tx.Exec(`INSERT INTO bug_notes (bug_id, body, created_at) VALUES (?, ?, ?)`, targetID, legacyNote, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO bug_imports (source, legacy_id, bug_id, imported_at) VALUES ('jobs.db', ?, ?, ?)`, bug.ID, targetID, now); err != nil {
		return err
	}
	return tx.Commit()
}

func findBugForLegacyImport(tx *sql.Tx, bug Bug) (int64, error) {
	if strings.TrimSpace(bug.Fingerprint) == "" {
		return 0, nil
	}
	var id int64
	err := tx.QueryRow(`SELECT id FROM bugs WHERE fingerprint = ? ORDER BY CASE WHEN status = 'open' THEN 0 ELSE 1 END, id LIMIT 1`, bug.Fingerprint).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}

func insertImportedBug(tx *sql.Tx, bug Bug) (int64, error) {
	result, err := tx.Exec(`
		INSERT INTO bugs
		    (status, title, kind, scope, likelihood, severity, fingerprint,
		     job_id, host, summary, detail, occurrences, created_at, updated_at,
		     closed_at, close_reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		defaultTrim(bug.Status, "open"),
		bug.Title, defaultTrim(bug.Kind, "bug"), defaultTrim(bug.Scope, "infrastructure"),
		defaultTrim(bug.Likelihood, "unknown"), defaultTrim(bug.Severity, "notice"),
		bug.Fingerprint, nullableBugInt64(bug.JobID), bug.Host, bug.Summary, bug.Detail,
		bug.Occurrences, bug.CreatedAt, bug.UpdatedAt, nullableBugInt64(bug.ClosedAt), bug.CloseReason,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func mergeImportedBug(tx *sql.Tx, id int64, bug Bug) error {
	_, err := tx.Exec(`
		UPDATE bugs
		   SET occurrences = MAX(occurrences, ?),
		       updated_at = MAX(updated_at, ?),
		       job_id = COALESCE(job_id, ?),
		       host = CASE WHEN host = '' THEN ? ELSE host END,
		       summary = CASE WHEN summary = '' THEN ? ELSE summary END,
		       detail = CASE WHEN detail = '' THEN ? ELSE detail END
		 WHERE id = ?`,
		bug.Occurrences, bug.UpdatedAt, nullableBugInt64(bug.JobID), bug.Host, bug.Summary, bug.Detail, id,
	)
	return err
}

func importLegacyBugNotes(tx *sql.Tx, legacy *sql.DB, legacyBugID, targetBugID int64) error {
	rows, err := legacy.Query(`SELECT body, created_at FROM bug_notes WHERE bug_id = ? ORDER BY created_at ASC, id ASC`, legacyBugID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var body string
		var createdAt int64
		if err := rows.Scan(&body, &createdAt); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO bug_notes (bug_id, body, created_at) VALUES (?, ?, ?)`, targetBugID, body, createdAt); err != nil {
			return err
		}
	}
	return rows.Err()
}

func ReportBug(database *sql.DB, report BugReport) (*Bug, bool, error) {
	if database == nil {
		return nil, false, errors.New("nil database")
	}
	now := time.Now().Unix()
	normalizeBugReport(&report)
	if report.Title == "" {
		return nil, false, errors.New("bug title is required")
	}
	if report.Fingerprint == "" {
		report.Fingerprint = report.Title
	}

	tx, err := database.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	var id int64
	err = tx.QueryRow(`SELECT id FROM bugs WHERE status = 'open' AND fingerprint = ?`, report.Fingerprint).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	created := false
	if errors.Is(err, sql.ErrNoRows) {
		result, err := tx.Exec(`
			INSERT INTO bugs
			    (status, title, kind, scope, likelihood, severity, fingerprint,
			     job_id, host, summary, detail, occurrences, created_at, updated_at)
			VALUES ('open', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			report.Title, report.Kind, report.Scope, report.Likelihood, report.Severity,
			report.Fingerprint, nullableBugInt64(report.JobID), report.Host, report.Summary, report.Detail, now, now,
		)
		if err != nil {
			return nil, false, err
		}
		id, err = result.LastInsertId()
		if err != nil {
			return nil, false, err
		}
		created = true
	} else {
		_, err := tx.Exec(`
			UPDATE bugs
			   SET occurrences = occurrences + 1,
			       updated_at = ?,
			       job_id = COALESCE(?, job_id),
			       host = CASE WHEN ? != '' THEN ? ELSE host END,
			       summary = CASE WHEN ? != '' THEN ? ELSE summary END,
			       detail = CASE WHEN ? != '' THEN ? ELSE detail END
			 WHERE id = ?`,
			now,
			nullableBugInt64(report.JobID),
			report.Host, report.Host,
			report.Summary, report.Summary,
			report.Detail, report.Detail,
			id,
		)
		if err != nil {
			return nil, false, err
		}
	}
	if strings.TrimSpace(report.Note) != "" {
		if _, err := tx.Exec(`INSERT INTO bug_notes (bug_id, body, created_at) VALUES (?, ?, ?)`, id, strings.TrimSpace(report.Note), now); err != nil {
			return nil, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	bug, err := GetBug(database, id)
	return bug, created, err
}

func nullableBugInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func AddBugNote(database *sql.DB, id int64, body string) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return errors.New("note body is required")
	}
	if _, err := GetBug(database, id); err != nil {
		return err
	}
	result, err := database.Exec(`INSERT INTO bug_notes (bug_id, body, created_at) VALUES (?, ?, ?)`, id, body, time.Now().Unix())
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func CloseBug(database *sql.DB, id int64, reason string) error {
	result, err := database.Exec(`
		UPDATE bugs
		   SET status = 'closed', closed_at = ?, close_reason = ?, updated_at = ?
		 WHERE id = ? AND status = 'open'`,
		time.Now().Unix(), strings.TrimSpace(reason), time.Now().Unix(), id,
	)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func GetBug(database *sql.DB, id int64) (*Bug, error) {
	rows, err := database.Query(`
		SELECT id, status, title, kind, scope, likelihood, severity, fingerprint,
		       job_id, host, summary, detail, occurrences, created_at, updated_at,
		       closed_at, close_reason
		  FROM bugs WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bugs, err := scanBugs(rows)
	if err != nil {
		return nil, err
	}
	if len(bugs) == 0 {
		return nil, sql.ErrNoRows
	}
	return &bugs[0], nil
}

func ListBugs(database *sql.DB, includeClosed bool) ([]Bug, error) {
	query := `
		SELECT id, status, title, kind, scope, likelihood, severity, fingerprint,
		       job_id, host, summary, detail, occurrences, created_at, updated_at,
		       closed_at, close_reason
		  FROM bugs`
	if !includeClosed {
		query += ` WHERE status = 'open'`
	}
	query += ` ORDER BY status ASC, updated_at DESC, id DESC`
	rows, err := database.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBugs(rows)
}

func ListBugNotes(database *sql.DB, bugID int64) ([]BugNote, error) {
	rows, err := database.Query(`SELECT id, bug_id, body, created_at FROM bug_notes WHERE bug_id = ? ORDER BY created_at ASC, id ASC`, bugID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notes []BugNote
	for rows.Next() {
		var note BugNote
		if err := rows.Scan(&note.ID, &note.BugID, &note.Body, &note.CreatedAt); err != nil {
			return nil, err
		}
		notes = append(notes, note)
	}
	return notes, rows.Err()
}

func normalizeBugReport(report *BugReport) {
	report.Title = strings.TrimSpace(report.Title)
	report.Kind = defaultTrim(report.Kind, "bug")
	report.Scope = defaultTrim(report.Scope, "infrastructure")
	report.Likelihood = defaultTrim(report.Likelihood, "unknown")
	report.Severity = defaultTrim(report.Severity, "notice")
	report.Fingerprint = strings.TrimSpace(report.Fingerprint)
	report.Host = strings.TrimSpace(report.Host)
	report.Summary = strings.TrimSpace(report.Summary)
	report.Detail = strings.TrimSpace(report.Detail)
	report.Note = strings.TrimSpace(report.Note)
}

func defaultTrim(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func scanBugs(rows *sql.Rows) ([]Bug, error) {
	var bugs []Bug
	for rows.Next() {
		var bug Bug
		var jobID sql.NullInt64
		var closedAt sql.NullInt64
		var host, summary, detail, closeReason sql.NullString
		if err := rows.Scan(
			&bug.ID, &bug.Status, &bug.Title, &bug.Kind, &bug.Scope, &bug.Likelihood, &bug.Severity,
			&bug.Fingerprint, &jobID, &host, &summary, &detail, &bug.Occurrences,
			&bug.CreatedAt, &bug.UpdatedAt, &closedAt, &closeReason,
		); err != nil {
			return nil, err
		}
		if jobID.Valid {
			bug.JobID = &jobID.Int64
		}
		if closedAt.Valid {
			bug.ClosedAt = &closedAt.Int64
		}
		bug.Host = host.String
		bug.Summary = summary.String
		bug.Detail = detail.String
		bug.CloseReason = closeReason.String
		bugs = append(bugs, bug)
	}
	return bugs, rows.Err()
}
