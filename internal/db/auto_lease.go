package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// AcquireAutoLease attempts to acquire (or renew) a scoped lease for the caller.
// It returns true when the lease is held by owner after this call.
func AcquireAutoLease(database *sql.DB, scope string, owner string, ttl time.Duration) (bool, error) {
	if database == nil || scope == "" || owner == "" || ttl <= 0 {
		return false, nil
	}
	now := time.Now().Unix()
	expiresAt := now + int64(ttl/time.Second)
	if expiresAt <= now {
		expiresAt = now + 1
	}

	tx, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var currentOwner string
	var currentExpires int64
	row := tx.QueryRow(`SELECT owner, expires_at FROM auto_leases WHERE scope = ?`, scope)
	switch err := row.Scan(&currentOwner, &currentExpires); {
	case err == nil:
		if currentOwner != owner && currentExpires > now {
			return false, nil
		}
	case errors.Is(err, sql.ErrNoRows):
		// No current owner.
	case isNoSuchTable(err):
		// Mixed-version DB: best-effort schema repair, then fail open if unavailable.
		if ensureErr := ensureAutoLeasesTable(database); ensureErr != nil {
			if shouldFailOpenLease(ensureErr) {
				return true, nil
			}
			return false, ensureErr
		}
		return AcquireAutoLease(database, scope, owner, ttl)
	default:
		return false, err
	}

	if _, err := tx.Exec(`
		INSERT INTO auto_leases(scope, owner, expires_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(scope) DO UPDATE SET
			owner = excluded.owner,
			expires_at = excluded.expires_at,
			updated_at = excluded.updated_at
	`, scope, owner, expiresAt, now); err != nil {
		if isNoSuchTable(err) {
			if ensureErr := ensureAutoLeasesTable(database); ensureErr != nil {
				if shouldFailOpenLease(ensureErr) {
					return true, nil
				}
				return false, ensureErr
			}
			return AcquireAutoLease(database, scope, owner, ttl)
		}
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// ReleaseAutoLease releases a scoped lease if it is owned by owner.
func ReleaseAutoLease(database *sql.DB, scope string, owner string) error {
	if database == nil || scope == "" || owner == "" {
		return nil
	}
	_, err := database.Exec(`DELETE FROM auto_leases WHERE scope = ? AND owner = ?`, scope, owner)
	if isNoSuchTable(err) {
		return nil
	}
	return err
}

func ensureAutoLeasesTable(database *sql.DB) error {
	_, err := database.Exec(`
		CREATE TABLE IF NOT EXISTS auto_leases (
			scope TEXT PRIMARY KEY,
			owner TEXT NOT NULL,
			expires_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_auto_leases_expires ON auto_leases(expires_at);
	`)
	return err
}

func shouldFailOpenLease(err error) bool {
	if err == nil {
		return false
	}
	e := strings.ToLower(err.Error())
	return strings.Contains(e, "readonly") || strings.Contains(e, "read-only")
}
