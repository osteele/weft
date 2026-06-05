package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const bugPrefix = "wb"

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
