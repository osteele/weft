#!/bin/bash
#
# Database helper functions for remote-jobs
# Stores job metadata in SQLite database at ~/.config/remote-jobs/jobs.db
#

# Database location
DB_DIR="${HOME}/.config/remote-jobs"
DB_FILE="${DB_DIR}/jobs.db"

# Ensure database directory and schema exist
db_init() {
    mkdir -p "$DB_DIR"
    sqlite3 "$DB_FILE" <<'EOF'
CREATE TABLE IF NOT EXISTS jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    host TEXT NOT NULL,
    session_name TEXT NOT NULL,
    working_dir TEXT NOT NULL,
    command TEXT NOT NULL,
    description TEXT,
    start_time INTEGER NOT NULL,
    end_time INTEGER,
    exit_code INTEGER,
    status TEXT NOT NULL DEFAULT 'running',
    UNIQUE(host, session_name, start_time)
);

CREATE INDEX IF NOT EXISTS idx_jobs_host ON jobs(host);
CREATE INDEX IF NOT EXISTS idx_jobs_session ON jobs(session_name);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_jobs_start ON jobs(start_time DESC);
EOF
}

# Record a new job start
# Usage: db_record_start <host> <session_name> <working_dir> <command> <start_time> [description]
db_record_start() {
    local host="$1"
    local session_name="$2"
    local working_dir="$3"
    local command="$4"
    local start_time="$5"
    local description="${6:-}"

    db_init

    # Use printf %q for safer escaping, but sqlite3 handles it via parameter binding
    # Use single sqlite3 call to ensure last_insert_rowid() returns the correct ID
    sqlite3 "$DB_FILE" "INSERT INTO jobs (host, session_name, working_dir, command, description, start_time, status)
        VALUES ('$(echo "$host" | sed "s/'/''/g")',
                '$(echo "$session_name" | sed "s/'/''/g")',
                '$(echo "$working_dir" | sed "s/'/''/g")',
                '$(echo "$command" | sed "s/'/''/g")',
                '$(echo "$description" | sed "s/'/''/g")',
                $start_time,
                'running');
        SELECT last_insert_rowid();"
}

# Update job completion status
# Usage: db_record_completion <host> <session_name> <exit_code> <end_time> [job_status]
db_record_completion() {
    local host="$1"
    local session_name="$2"
    local exit_code="$3"
    local end_time="$4"
    local job_status="${5:-completed}"

    db_init

    # Update the most recent running job with this host/session
    sqlite3 "$DB_FILE" "UPDATE jobs
        SET exit_code = $exit_code,
            end_time = $end_time,
            status = '$job_status'
        WHERE id = (
            SELECT id FROM jobs
            WHERE host = '$(echo "$host" | sed "s/'/''/g")'
              AND session_name = '$(echo "$session_name" | sed "s/'/''/g")'
              AND status = 'running'
            ORDER BY start_time DESC
            LIMIT 1
        );"
}

# Record a pending job (job that couldn't start due to connection failure)
# Usage: db_record_pending <host> <session_name> <working_dir> <command> <description>
db_record_pending() {
    local host="$1"
    local session_name="$2"
    local working_dir="$3"
    local command="$4"
    local description="${5:-}"
    local start_time
    start_time=$(date +%s)

    db_init

    sqlite3 "$DB_FILE" "INSERT INTO jobs (host, session_name, working_dir, command, description, start_time, status)
        VALUES ('$(echo "$host" | sed "s/'/''/g")',
                '$(echo "$session_name" | sed "s/'/''/g")',
                '$(echo "$working_dir" | sed "s/'/''/g")',
                '$(echo "$command" | sed "s/'/''/g")',
                '$(echo "$description" | sed "s/'/''/g")',
                $start_time,
                'pending');
        SELECT last_insert_rowid();"
}

# Get a pending job by ID
# Usage: db_get_pending_job <id>
db_get_pending_job() {
    local id="$1"

    db_init

    sqlite3 -separator $'\t' "$DB_FILE" "SELECT * FROM jobs WHERE id = $id AND status = 'pending';"
}

# List pending jobs, optionally filtered by host
# Usage: db_list_pending [host]
db_list_pending() {
    local host="${1:-}"

    db_init

    local where_clause="WHERE status = 'pending'"
    if [ -n "$host" ]; then
        where_clause="$where_clause AND host = '$(echo "$host" | sed "s/'/''/g")'"
    fi

    sqlite3 -header -column "$DB_FILE" "SELECT
        id,
        host,
        session_name,
        substr(description, 1, 40) as description,
        datetime(start_time, 'unixepoch', 'localtime') as queued
        FROM jobs
        $where_clause
        ORDER BY start_time DESC;"
}

# Mark a pending job as started (transitions to running)
# Usage: db_mark_started <id>
db_mark_started() {
    local id="$1"
    local start_time
    start_time=$(date +%s)

    db_init

    sqlite3 "$DB_FILE" "UPDATE jobs
        SET status = 'running',
            start_time = $start_time
        WHERE id = $id AND status = 'pending';"
}

# Delete a pending job
# Usage: db_delete_pending <id>
db_delete_pending() {
    local id="$1"

    db_init

    sqlite3 "$DB_FILE" "DELETE FROM jobs WHERE id = $id AND status = 'pending';"
}

# Mark job as dead (detected as no longer running without completing normally)
# Usage: db_mark_dead <host> <session_name>
db_mark_dead() {
    local host="$1"
    local session_name="$2"
    local end_time
    end_time=$(date +%s)

    db_init

    sqlite3 "$DB_FILE" "UPDATE jobs
        SET end_time = $end_time,
            status = 'dead'
        WHERE host = '$(echo "$host" | sed "s/'/''/g")'
          AND session_name = '$(echo "$session_name" | sed "s/'/''/g")'
          AND status = 'running';"
}

# Get job status from database
# Usage: db_get_status <host> <session_name>
# Returns: running|completed|dead or empty if not found
db_get_status() {
    local host="$1"
    local session_name="$2"

    db_init

    sqlite3 "$DB_FILE" "SELECT status FROM jobs
        WHERE host = '$(echo "$host" | sed "s/'/''/g")'
          AND session_name = '$(echo "$session_name" | sed "s/'/''/g")'
        ORDER BY start_time DESC
        LIMIT 1;"
}

# Get full job info from database
# Usage: db_get_job <host> <session_name>
# Returns: tab-separated: id|host|session_name|working_dir|command|description|start_time|end_time|exit_code|status
db_get_job() {
    local host="$1"
    local session_name="$2"

    db_init

    sqlite3 -separator $'\t' "$DB_FILE" "SELECT * FROM jobs
        WHERE host = '$(echo "$host" | sed "s/'/''/g")'
          AND session_name = '$(echo "$session_name" | sed "s/'/''/g")'
        ORDER BY start_time DESC
        LIMIT 1;"
}

# Get job by ID
# Usage: db_get_job_by_id <id>
db_get_job_by_id() {
    local id="$1"

    db_init

    sqlite3 -separator $'\t' "$DB_FILE" "SELECT * FROM jobs WHERE id = $id;"
}

# List all jobs with optional filters
# Usage: db_list_jobs [filter_status] [host] [limit]
db_list_jobs() {
    local filter_status="${1:-}"
    local host="${2:-}"
    local limit="${3:-50}"

    db_init

    local where_clause=""
    local conditions=()

    if [ -n "$filter_status" ]; then
        conditions+=("status = '$(echo "$filter_status" | sed "s/'/''/g")'")
    fi

    if [ -n "$host" ]; then
        conditions+=("host = '$(echo "$host" | sed "s/'/''/g")'")
    fi

    if [ ${#conditions[@]} -gt 0 ]; then
        where_clause="WHERE $(IFS=' AND '; echo "${conditions[*]}")"
    fi

    sqlite3 -header -column "$DB_FILE" "SELECT
        id,
        host,
        session_name,
        substr(description, 1, 40) as description,
        datetime(start_time, 'unixepoch', 'localtime') as started,
        CASE
            WHEN end_time IS NOT NULL THEN datetime(end_time, 'unixepoch', 'localtime')
            ELSE '-'
        END as ended,
        CASE
            WHEN exit_code IS NULL THEN '-'
            ELSE exit_code
        END as exit,
        status
        FROM jobs
        $where_clause
        ORDER BY start_time DESC
        LIMIT $limit;"
}

# List recent jobs (convenience wrapper)
# Usage: db_list_recent [limit]
db_list_recent() {
    local limit="${1:-10}"
    db_list_jobs "" "" "$limit"
}

# List running jobs
# Usage: db_list_running [host]
db_list_running() {
    local host="${1:-}"
    db_list_jobs "running" "$host" 100
}

# Search jobs by description or command
# Usage: db_search_jobs <query> [limit]
db_search_jobs() {
    local query="$1"
    local limit="${2:-50}"

    db_init

    sqlite3 -header -column "$DB_FILE" "SELECT
        id,
        host,
        session_name,
        substr(description, 1, 40) as description,
        datetime(start_time, 'unixepoch', 'localtime') as started,
        status
        FROM jobs
        WHERE description LIKE '%$(echo "$query" | sed "s/'/''/g")%'
           OR command LIKE '%$(echo "$query" | sed "s/'/''/g")%'
        ORDER BY start_time DESC
        LIMIT $limit;"
}

# Get detailed job info formatted for display
# Usage: db_show_job <host> <session_name>
db_show_job() {
    local host="$1"
    local session_name="$2"

    db_init

    sqlite3 "$DB_FILE" "SELECT
        'ID: ' || id || char(10) ||
        'Host: ' || host || char(10) ||
        'Session: ' || session_name || char(10) ||
        'Working Dir: ' || working_dir || char(10) ||
        'Command: ' || command || char(10) ||
        'Description: ' || COALESCE(description, '(none)') || char(10) ||
        'Started: ' || datetime(start_time, 'unixepoch', 'localtime') || char(10) ||
        'Ended: ' || COALESCE(datetime(end_time, 'unixepoch', 'localtime'), '(still running)') || char(10) ||
        'Exit Code: ' || COALESCE(exit_code, '-') || char(10) ||
        'Status: ' || status
        FROM jobs
        WHERE host = '$(echo "$host" | sed "s/'/''/g")'
          AND session_name = '$(echo "$session_name" | sed "s/'/''/g")'
        ORDER BY start_time DESC
        LIMIT 1;"
}

# Delete old completed jobs
# Usage: db_cleanup_old <days>
db_cleanup_old() {
    local days="${1:-30}"
    local cutoff
    cutoff=$(( $(date +%s) - (days * 86400) ))

    db_init

    local count
    count=$(sqlite3 "$DB_FILE" "SELECT COUNT(*) FROM jobs
        WHERE status != 'running' AND start_time < $cutoff;")

    sqlite3 "$DB_FILE" "DELETE FROM jobs
        WHERE status != 'running' AND start_time < $cutoff;"

    echo "Deleted $count old job records"
}
