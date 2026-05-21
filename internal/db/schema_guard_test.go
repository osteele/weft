package db

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// schemaGoldenPath is the recorded fingerprint of the table schema produced by
// the squashed baseline (plus any later migrations), relative to this package
// directory (the test CWD).
const schemaGoldenPath = "testdata/schema.txt"

// TestSchemaMatchesGolden fingerprints the tables of a freshly-migrated
// database and compares the result to testdata/schema.txt. Any unintended
// schema change — a column, constraint, foreign key or unique index that
// drifted without a corresponding migration — fails the test.
//
// When a migration legitimately changes the schema, regenerate the golden:
//
//	WEFT_UPDATE_SCHEMA_GOLDEN=1 go test ./internal/db/ -run TestSchemaMatchesGolden
func TestSchemaMatchesGolden(t *testing.T) {
	db := SetupTestDB(t)

	live, err := fingerprintTableSchema(db)
	if err != nil {
		t.Fatalf("fingerprintTableSchema: %v", err)
	}
	live = strings.TrimSpace(live)

	if os.Getenv("WEFT_UPDATE_SCHEMA_GOLDEN") != "" {
		if err := os.WriteFile(schemaGoldenPath, []byte(live+"\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", schemaGoldenPath, err)
		}
		t.Logf("updated %s", schemaGoldenPath)
		return
	}

	want, err := os.ReadFile(schemaGoldenPath)
	if err != nil {
		t.Fatalf("read %s (regenerate with WEFT_UPDATE_SCHEMA_GOLDEN=1): %v", schemaGoldenPath, err)
	}
	wantStr := strings.TrimSpace(string(want))
	if wantStr != live {
		t.Fatalf("table schema drifted from %s.\n\n"+
			"If this is an intended migration, regenerate the golden:\n"+
			"  WEFT_UPDATE_SCHEMA_GOLDEN=1 go test ./internal/db/ -run TestSchemaMatchesGolden\n\n"+
			"schema diff (golden -> live):\n%s",
			schemaGoldenPath, diffLines(wantStr, live))
	}
}

// TestValidateViews_DetectsBrokenView confirms validateViews (run at the end
// of startupRepair on every Open) fails loudly when a view references a column
// that does not exist — the symptom of a schema change missing its migration.
func TestValidateViews_DetectsBrokenView(t *testing.T) {
	db := SetupTestDB(t)

	if _, err := db.Exec(`CREATE VIEW broken_probe_view AS SELECT no_such_col FROM jobs`); err != nil {
		t.Fatalf("create broken view: %v", err)
	}

	err := validateViews(db)
	if err == nil {
		t.Fatal("validateViews returned nil for a view referencing a missing column")
	}
	if !strings.Contains(err.Error(), "broken_probe_view") {
		t.Fatalf("validateViews error should name the broken view, got: %v", err)
	}
}

// --- table schema fingerprint --------------------------------------------

// fingerprintTableSchema renders a deterministic, human-readable description
// of every persistent table. Columns, foreign keys and unique indexes are
// derived from PRAGMA introspection, so the output is immune to DDL
// formatting and to the column text SQLite appends to sqlite_master.sql after
// ALTER TABLE ADD COLUMN. CHECK constraints (which PRAGMA cannot report) are
// extracted from the stored CREATE TABLE text and whitespace-normalized.
func fingerprintTableSchema(db *sql.DB) (string, error) {
	rows, err := db.Query(`SELECT name, sql FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		  AND name != 'goose_db_version' ORDER BY name`)
	if err != nil {
		return "", fmt.Errorf("list tables: %w", err)
	}
	type tableRow struct{ name, createSQL string }
	var tables []tableRow
	for rows.Next() {
		var name string
		var createSQL sql.NullString
		if err := rows.Scan(&name, &createSQL); err != nil {
			rows.Close()
			return "", fmt.Errorf("scan table row: %w", err)
		}
		tables = append(tables, tableRow{name: name, createSQL: createSQL.String})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", fmt.Errorf("iterate tables: %w", err)
	}
	rows.Close()

	var b strings.Builder
	for _, t := range tables {
		fmt.Fprintf(&b, "table %s\n", t.name)
		lines, err := tableColumnLines(db, t.name)
		if err != nil {
			return "", err
		}
		for _, l := range lines {
			b.WriteString("  " + l + "\n")
		}
		fkLines, err := tableForeignKeyLines(db, t.name)
		if err != nil {
			return "", err
		}
		for _, l := range fkLines {
			b.WriteString("  " + l + "\n")
		}
		idxLines, err := tableUniqueIndexLines(db, t.name)
		if err != nil {
			return "", err
		}
		for _, l := range idxLines {
			b.WriteString("  " + l + "\n")
		}
		for _, c := range extractCheckClauses(t.createSQL) {
			b.WriteString("  check " + c + "\n")
		}
	}
	return b.String(), nil
}

func tableColumnLines(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info("%s")`, table))
	if err != nil {
		return nil, fmt.Errorf("table_info(%s): %w", table, err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var cid, notnull, pk int
		var name string
		var ctype, dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("scan table_info(%s): %w", table, err)
		}
		def := "<none>"
		if dflt.Valid {
			def = dflt.String
		}
		lines = append(lines, fmt.Sprintf("col %d %s %s notnull=%d default=%s pk=%d",
			cid, name, ctype.String, notnull, def, pk))
	}
	return lines, rows.Err()
}

func tableForeignKeyLines(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA foreign_key_list("%s")`, table))
	if err != nil {
		return nil, fmt.Errorf("foreign_key_list(%s): %w", table, err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id, seq int
		var refTable, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &refTable, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return nil, fmt.Errorf("scan foreign_key_list(%s): %w", table, err)
		}
		lines = append(lines, fmt.Sprintf("fk %s -> %s(%s) on_update=%s on_delete=%s",
			from, refTable, to, onUpdate, onDelete))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(lines)
	return lines, nil
}

func tableUniqueIndexLines(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA index_list("%s")`, table))
	if err != nil {
		return nil, fmt.Errorf("index_list(%s): %w", table, err)
	}
	type idx struct {
		name, origin string
		partial      int
	}
	var unique []idx
	for rows.Next() {
		var seq, isUnique, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &isUnique, &origin, &partial); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan index_list(%s): %w", table, err)
		}
		if isUnique == 1 {
			unique = append(unique, idx{name: name, origin: origin, partial: partial})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	var lines []string
	for _, ix := range unique {
		cols, err := indexColumns(db, ix.name)
		if err != nil {
			return nil, err
		}
		// The index name itself is omitted: SQLite auto-index names
		// (sqlite_autoindex_*) are positional artifacts. The column set,
		// origin and partial flag describe the constraint semantically.
		lines = append(lines, fmt.Sprintf("unique-index (%s) origin=%s partial=%d",
			strings.Join(cols, ","), ix.origin, ix.partial))
	}
	sort.Strings(lines)
	return lines, nil
}

func indexColumns(db *sql.DB, indexName string) ([]string, error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA index_info("%s")`, indexName))
	if err != nil {
		return nil, fmt.Errorf("index_info(%s): %w", indexName, err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var seqno, cid int
		var name sql.NullString
		if err := rows.Scan(&seqno, &cid, &name); err != nil {
			return nil, fmt.Errorf("scan index_info(%s): %w", indexName, err)
		}
		cols = append(cols, name.String)
	}
	return cols, rows.Err()
}

// extractCheckClauses returns every CHECK(...) expression in a CREATE TABLE
// statement (table-level and column-level, named and unnamed), each
// whitespace-normalized, sorted.
func extractCheckClauses(createSQL string) []string {
	var out []string
	for i := 0; i+5 <= len(createSQL); i++ {
		if createSQL[i:i+5] != "CHECK" {
			continue
		}
		if i > 0 && isIdentChar(createSQL[i-1]) {
			continue
		}
		j := i + 5
		for j < len(createSQL) && isSpace(createSQL[j]) {
			j++
		}
		if j >= len(createSQL) || createSQL[j] != '(' {
			continue
		}
		depth, end := 0, -1
		for k := j; k < len(createSQL); k++ {
			if createSQL[k] == '(' {
				depth++
			} else if createSQL[k] == ')' {
				depth--
				if depth == 0 {
					end = k
					break
				}
			}
		}
		if end < 0 {
			continue
		}
		out = append(out, normalizeWS(createSQL[j:end+1]))
		i = end
	}
	sort.Strings(out)
	return out
}

func isIdentChar(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func normalizeWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// diffLines renders a compact set-difference of two multi-line strings.
func diffLines(recorded, live string) string {
	inRecorded := map[string]bool{}
	for _, l := range strings.Split(recorded, "\n") {
		inRecorded[l] = true
	}
	inLive := map[string]bool{}
	for _, l := range strings.Split(live, "\n") {
		inLive[l] = true
	}
	var out []string
	for _, l := range strings.Split(recorded, "\n") {
		if !inLive[l] {
			out = append(out, "- "+l)
		}
	}
	for _, l := range strings.Split(live, "\n") {
		if !inRecorded[l] {
			out = append(out, "+ "+l)
		}
	}
	if len(out) == 0 {
		return "(no line-level difference; whitespace only)"
	}
	return strings.Join(out, "\n")
}
