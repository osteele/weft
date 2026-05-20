package db

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// schemaHistoryPath is the append-only record of the table schema at each
// schema version, relative to this package directory (the test CWD).
const schemaHistoryPath = "testdata/schema_history.txt"

// TestTableSchemaMatchesHistory enforces that every change to a persistent
// table's columns or constraints is recorded against a fresh schema version.
//
// It fingerprints the tables of a freshly-initialized database and compares
// the result to the last section of testdata/schema_history.txt. The history
// file is append-only with create-only regeneration (see updateSchemaHistory):
// the only way to record a changed table schema is a new currentSchemaVersion,
// which can only come from appending a versionedMigrations entry. This closes
// the recurring bug where a column was added without a migration, so existing
// databases fast-pathed past the migration and broke at runtime.
//
// Regenerate with: just regen-schema-golden
func TestTableSchemaMatchesHistory(t *testing.T) {
	db := SetupTestDB(t)

	live, err := fingerprintTableSchema(db)
	if err != nil {
		t.Fatalf("fingerprintTableSchema: %v", err)
	}
	live = strings.TrimSpace(live)

	sections, err := readSchemaHistory(schemaHistoryPath)
	if err != nil {
		t.Fatalf("read %s: %v", schemaHistoryPath, err)
	}

	if os.Getenv("WEFT_UPDATE_SCHEMA_HISTORY") != "" {
		updated, err := updateSchemaHistory(schemaHistoryPath, sections, currentSchemaVersion, live)
		if err != nil {
			t.Fatalf("update schema history: %v", err)
		}
		if updated {
			t.Logf("appended [version %d] to %s", currentSchemaVersion, schemaHistoryPath)
		} else {
			t.Logf("%s already current at version %d", schemaHistoryPath, currentSchemaVersion)
		}
		return
	}

	if len(sections) == 0 {
		t.Fatalf("%s is empty — run `just regen-schema-golden` to create the first section", schemaHistoryPath)
	}
	last := sections[len(sections)-1]

	if last.version != currentSchemaVersion {
		t.Fatalf("%s last records version %d but currentSchemaVersion is %d.\n"+
			"If you added a versionedMigrations entry, run `just regen-schema-golden` "+
			"to append a [version %d] section.",
			schemaHistoryPath, last.version, currentSchemaVersion, currentSchemaVersion)
	}

	if last.body != live {
		t.Fatalf("table schema for version %d changed but currentSchemaVersion did not bump.\n\n"+
			"A table column or constraint changed. Add a versionedMigrations entry "+
			"(this bumps currentSchemaVersion via migrations.go), then run "+
			"`just regen-schema-golden`.\n"+
			"Do NOT edit the existing [version %d] section of %s.\n\n"+
			"schema diff (recorded -> live):\n%s",
			currentSchemaVersion, currentSchemaVersion, schemaHistoryPath,
			diffLines(last.body, live))
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
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
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

// --- append-only schema history file -------------------------------------

type schemaSection struct {
	version int
	body    string
}

// readSchemaHistory parses the version-delimited history file. A missing file
// yields an empty slice so the first regeneration can create it.
func readSchemaHistory(path string) ([]schemaSection, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sections []schemaSection
	var cur *schemaSection
	var body []string
	flush := func() {
		if cur != nil {
			cur.body = strings.TrimSpace(strings.Join(body, "\n"))
			sections = append(sections, *cur)
		}
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := parseVersionHeader(line); ok {
			flush()
			cur = &schemaSection{version: v}
			body = nil
			continue
		}
		if cur != nil {
			body = append(body, line)
		}
	}
	flush()
	return sections, nil
}

func parseVersionHeader(line string) (int, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "[version ") || !strings.HasSuffix(line, "]") {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(line[len("[version ") : len(line)-1]))
	if err != nil {
		return 0, false
	}
	return v, true
}

// updateSchemaHistory appends a new [version] section for the current schema
// version. Regeneration is create-only: an existing section is never
// rewritten. If the schema changed but the version did not bump, this refuses
// — forcing the developer to add a versionedMigrations entry first. Returns
// true if a section was appended.
func updateSchemaHistory(path string, sections []schemaSection, version int, body string) (bool, error) {
	if len(sections) > 0 {
		last := sections[len(sections)-1]
		if last.version > version {
			return false, fmt.Errorf("%s records version %d, newer than currentSchemaVersion %d",
				path, last.version, version)
		}
		if last.version == version {
			if last.body == body {
				return false, nil
			}
			return false, fmt.Errorf("version %d is already recorded in %s and is frozen; "+
				"a table changed — add a versionedMigrations entry (which bumps "+
				"currentSchemaVersion) before regenerating", version, path)
		}
	}
	sections = append(sections, schemaSection{version: version, body: body})
	return true, writeSchemaHistory(path, sections)
}

func writeSchemaHistory(path string, sections []schemaSection) error {
	var b strings.Builder
	b.WriteString("# Append-only record of the table schema at each schema version.\n")
	b.WriteString("# Generated by TestTableSchemaMatchesHistory; regenerate with `just regen-schema-golden`.\n")
	b.WriteString("# Never edit an existing [version N] section by hand.\n")
	for _, s := range sections {
		b.WriteString("\n")
		fmt.Fprintf(&b, "[version %d]\n", s.version)
		b.WriteString(strings.TrimSpace(s.body))
		b.WriteString("\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
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
