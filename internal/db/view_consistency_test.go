package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCurrentJobStatusViewCopiesMatchMaterializedView(t *testing.T) {
	db := SetupTestDB(t)
	materialized := normalizedViewSQL(t, db, "job_status")

	for _, tc := range []struct {
		name string
		path string
		nth  int
	}{
		{
			name: "baseline schema copy",
			path: filepath.Join("migrations", "baseline_schema.sql"),
			nth:  1,
		},
		{
			name: "v18 effective source reused by repair migration",
			path: filepath.Join("migrations", "sql", "abandoned_attempts_v18.sql"),
			nth:  1,
		},
		{
			name: "v25 skypilot copy",
			path: filepath.Join("migrations", "sql", "00025_skypilot_external_jobs.sql"),
			nth:  1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := normalizedViewSource(t, tc.path, "job_status", tc.nth)
			if source != materialized {
				t.Fatalf("%s job_status view copy differs from materialized view.\n\nsource:\n%s\n\nmaterialized:\n%s\n\ndiff:\n%s",
					tc.path, source, materialized, diffLines(source, materialized))
			}
		})
	}
}

func TestCurrentAuthoritativeAttemptsViewMatchesRepairDefinition(t *testing.T) {
	db := SetupTestDB(t)
	materialized := normalizedViewSQL(t, db, "authoritative_job_attempts")
	want := normalizeViewSQL(`CREATE VIEW authoritative_job_attempts AS
		 SELECT ja.* FROM job_attempts ja
		  WHERE ja.abandoned_at IS NULL
		    AND NOT EXISTS (
		        SELECT 1 FROM move_intents mi
		         WHERE mi.state = 'open'
		           AND mi.id = ja.move_intent_id
		    )`)
	if materialized != want {
		t.Fatalf("authoritative_job_attempts differs from repair definition.\n\nwant:\n%s\n\ngot:\n%s\n\ndiff:\n%s",
			want, materialized, diffLines(want, materialized))
	}
}

func normalizedViewSQL(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var sqlText string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'view' AND name = ?`, name).Scan(&sqlText); err != nil {
		t.Fatalf("read sqlite_master view %s: %v", name, err)
	}
	return normalizeViewSQL(sqlText)
}

func normalizedViewSource(t *testing.T, path, viewName string, nth int) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sqlText, ok := extractCreateView(stripSQLLineComments(string(data)), viewName, nth)
	if !ok {
		t.Fatalf("%s does not contain occurrence %d of CREATE VIEW %s", path, nth, viewName)
	}
	return normalizeViewSQL(sqlText)
}

func extractCreateView(sqlText, viewName string, nth int) (string, bool) {
	markers := []string{
		"CREATE VIEW " + viewName + " AS",
		"CREATE VIEW IF NOT EXISTS " + viewName + " AS",
	}
	start := -1
	searchFrom := 0
	for i := 0; i < nth; i++ {
		idx := -1
		for _, marker := range markers {
			candidate := strings.Index(sqlText[searchFrom:], marker)
			if candidate >= 0 && (idx < 0 || candidate < idx) {
				idx = candidate
			}
		}
		if idx < 0 {
			return "", false
		}
		start = searchFrom + idx
		searchFrom = start + len("CREATE VIEW ")
	}
	end := strings.Index(sqlText[start:], ";")
	if end < 0 {
		return strings.TrimSpace(sqlText[start:]), true
	}
	return strings.TrimSpace(sqlText[start : start+end]), true
}

func normalizeViewSQL(sqlText string) string {
	sqlText = strings.TrimSpace(sqlText)
	sqlText = strings.TrimSuffix(sqlText, ";")
	sqlText = stripSQLLineComments(sqlText)
	sqlText = strings.ReplaceAll(sqlText, "%%", "%")
	sqlText = strings.Replace(sqlText, "CREATE VIEW IF NOT EXISTS ", "CREATE VIEW ", 1)
	return normalizeWS(sqlText)
}

func stripSQLLineComments(sqlText string) string {
	var lines []string
	for _, line := range strings.Split(sqlText, "\n") {
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
