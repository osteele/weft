-- Enforce the launches-table invariants at the DB layer via triggers:
--   * TerminalInstancesHaveEndTime    (campaign-lifecycle.allium)
--   * TerminalInstancesHaveReason     (campaign-lifecycle.allium)
--   * GraceRequiresDeadline           (campaign-lifecycle.allium)
--
-- Background. A prod-DB scan (2026-05-26) found:
--   *  5 terminal launches with NULL ended_at
--   * 49 failed/canceled launches with NULL termination_reason
--   *  0 grace launches missing grace_deadline / grace_started_at
--
-- The leaker for the reason invariant is the fallback branch of
-- internal/db/cloud_instances.go:999 — UPDATE launches SET status, ended_at
-- without a termination_reason argument — used when the caller doesn't
-- know the cause. Every such row makes job_status's status-derivation
-- query miss its expected mapping (see baseline_schema.sql line ~925),
-- which silently degrades the failure-mode classifier and analytics that
-- read this column.
--
-- This migration moves enforcement from "every writer MUST stamp these"
-- to "the table guarantees them," matching the pattern from 00004 + 00005
-- (placement intents) and 00006 (attempt closing fields).
--
-- Auto-derive vs RAISE per field:
--   * ended_at: auto-stamp on status→terminal. There is no
--     "agent-backfill carve-out" for launches the way job_attempts has
--     (launches are mutated only locally; ended_at always has a sane
--     wall-clock fallback).
--   * termination_reason: auto-derive 'unknown' on status→failed/canceled
--     when missing. 'unknown' is already in the CHECK enum (see
--     launches_termination_reason_check) and is the documented sentinel
--     for "writer didn't know the cause." Writers that DO pass a specific
--     reason are unaffected — the trigger's WHEN clause requires
--     termination_reason IS NULL.
--   * grace_started_at / grace_deadline: RAISE on status→grace without
--     both. Zero current violators, and the deadline is operationally
--     load-bearing — no sane auto-derived value exists. Forcing the
--     writer to pass them is the right call.

-- +goose Up
-- +goose StatementBegin
-- Backfill: terminal launches missing ended_at.
-- Use the latest known wall-clock anchor on the row, preferring more
-- recent stamps (provider_running_at > launched_at > ready_at > created_at).
UPDATE launches
   SET ended_at = COALESCE(
       NULLIF(provider_running_at, 0),
       NULLIF(launched_at, 0),
       NULLIF(ready_at, 0),
       NULLIF(created_at, 0),
       strftime('%s','now'))
 WHERE status IN ('failed','canceled','completed')
   AND (ended_at IS NULL OR ended_at = 0);
-- +goose StatementEnd

-- +goose StatementBegin
-- Backfill: failed/canceled launches missing termination_reason.
UPDATE launches
   SET termination_reason = CASE
       WHEN status = 'canceled'  THEN 'canceled'
       WHEN status = 'completed' THEN 'completed'
       ELSE 'unknown'
       END
 WHERE status IN ('failed','canceled')
   AND (termination_reason IS NULL OR termination_reason = '');
-- +goose StatementEnd

-- Trigger: auto-stamp ended_at when a launch transitions to terminal.
-- SQLite's recursive_triggers PRAGMA is OFF, so the inner UPDATE does
-- not re-fire this trigger. No open-mirror cleanup is needed (launches
-- have no derived open-set table the way job_attempts does).
-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS launches_terminal_auto_stamp_ended_at
AFTER UPDATE OF status ON launches
WHEN NEW.status IN ('failed','canceled','completed')
  AND OLD.status != NEW.status
  AND (NEW.ended_at IS NULL OR NEW.ended_at = 0)
BEGIN
    UPDATE launches SET ended_at = strftime('%s','now') WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS launches_terminal_auto_stamp_ended_at_insert
AFTER INSERT ON launches
WHEN NEW.status IN ('failed','canceled','completed')
  AND (NEW.ended_at IS NULL OR NEW.ended_at = 0)
BEGIN
    UPDATE launches SET ended_at = strftime('%s','now') WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- Trigger: auto-derive termination_reason on status→failed/canceled when
-- the writer didn't pass one. status='completed' maps to reason='completed';
-- status='canceled' maps to reason='canceled'; status='failed' falls back
-- to 'unknown' (the existing sentinel in launches_termination_reason_check).
-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS launches_terminal_auto_derive_reason
AFTER UPDATE OF status ON launches
WHEN NEW.status IN ('failed','canceled','completed')
  AND OLD.status != NEW.status
  AND (NEW.termination_reason IS NULL OR NEW.termination_reason = '')
BEGIN
    UPDATE launches
       SET termination_reason = CASE
           WHEN NEW.status = 'canceled'  THEN 'canceled'
           WHEN NEW.status = 'completed' THEN 'completed'
           ELSE 'unknown'
           END
     WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS launches_terminal_auto_derive_reason_insert
AFTER INSERT ON launches
WHEN NEW.status IN ('failed','canceled','completed')
  AND (NEW.termination_reason IS NULL OR NEW.termination_reason = '')
BEGIN
    UPDATE launches
       SET termination_reason = CASE
           WHEN NEW.status = 'canceled'  THEN 'canceled'
           WHEN NEW.status = 'completed' THEN 'completed'
           ELSE 'unknown'
           END
     WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- Trigger: RAISE when a launch transitions to 'grace' without both
-- grace_started_at and grace_deadline. Both are operationally
-- load-bearing — the agent's R2 grace-poll loop uses grace_deadline to
-- decide when to self-destruct — and no sane auto-derived deadline
-- exists at the table layer (the default grace period is a config
-- value, not a schema fact). Force writers to pass them.
-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS launches_grace_requires_deadline_update
BEFORE UPDATE OF status ON launches
WHEN NEW.status = 'grace'
  AND OLD.status != 'grace'
  AND (NEW.grace_started_at IS NULL OR NEW.grace_deadline IS NULL)
BEGIN
    SELECT RAISE(ABORT, 'launch transition to grace requires grace_started_at and grace_deadline (see GraceRequiresDeadline invariant)');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS launches_grace_requires_deadline_insert
BEFORE INSERT ON launches
WHEN NEW.status = 'grace'
  AND (NEW.grace_started_at IS NULL OR NEW.grace_deadline IS NULL)
BEGIN
    SELECT RAISE(ABORT, 'launch inserted in grace status requires grace_started_at and grace_deadline (see GraceRequiresDeadline invariant)');
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS launches_terminal_auto_stamp_ended_at;
DROP TRIGGER IF EXISTS launches_terminal_auto_stamp_ended_at_insert;
DROP TRIGGER IF EXISTS launches_terminal_auto_derive_reason;
DROP TRIGGER IF EXISTS launches_terminal_auto_derive_reason_insert;
DROP TRIGGER IF EXISTS launches_grace_requires_deadline_update;
DROP TRIGGER IF EXISTS launches_grace_requires_deadline_insert;
-- Backfill UPDATEs have no logical inverse.
-- +goose StatementEnd
