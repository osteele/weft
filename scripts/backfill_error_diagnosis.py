#!/usr/bin/env python3
"""Backfill job_attempts.error_diagnosis for historical failed attempts."""

from __future__ import annotations

import argparse
import json
import re
import sqlite3
import time
from pathlib import Path


PatternRule = tuple[str, str, str, float, re.Pattern[str]]

RULES: list[PatternRule] = [
    (
        "preempted",
        "environment",
        "Cloud instance was preempted",
        0.95,
        re.compile(
            r"preempt(?:ed|ion)|spot interruption|instance reclaimed|cloud_outcome[=:]\s*preempted",
            re.I | re.S,
        ),
    ),
    (
        "gpu_oom",
        "environment",
        "GPU out of memory",
        0.95,
        re.compile(
            r"(?:CUDA|HIP).*out of memory|torch\.cuda\.OutOfMemoryError", re.I | re.S
        ),
    ),
    (
        "cuda_error",
        "environment",
        "CUDA runtime error",
        0.9,
        re.compile(
            r"CUDA error|cuFFT error|cuBLAS error|CUDNN_STATUS_|CUDA_ERROR_[A-Z0-9_]+|illegal memory access",
            re.I | re.S,
        ),
    ),
    (
        "disk_full",
        "environment",
        "Disk full or quota exceeded",
        0.95,
        re.compile(
            r"No space left on device|disk quota exceeded|EDQUOT|ENOSPC", re.I | re.S
        ),
    ),
    (
        "ssh_disconnect",
        "environment",
        "SSH connection lost",
        0.85,
        re.compile(
            r"Connection reset|Broken pipe|Host is unreachable|No route to host|ssh:.*disconnect",
            re.I | re.S,
        ),
    ),
    (
        "timeout",
        "environment",
        "Execution timed out",
        0.9,
        re.compile(
            r"timed out|timeout|deadline exceeded|exceeded .*max(?:imum)? time|SIGTERM.*budget",
            re.I | re.S,
        ),
    ),
    (
        "module_not_found",
        "code",
        "Missing Python module or import",
        0.9,
        re.compile(
            r"ModuleNotFoundError:\s*No module named ['\"]([^'\"]+)['\"]|ImportError:|cannot import name|"
            r"(?:error while loading shared libraries|cannot open shared object file)",
            re.I | re.S,
        ),
    ),
    (
        "assert_failure",
        "code",
        "Assertion or runtime failure",
        0.8,
        re.compile(
            r"Traceback \(most recent call last\):.*(?:AssertionError|RuntimeError)",
            re.I | re.S,
        ),
    ),
    (
        "subprocess_failure",
        "code",
        "Subprocess exited non-zero",
        0.75,
        re.compile(
            r"subprocess\.(?:CalledProcessError|run|check_call|check_output)|Command .* returned non-zero exit status",
            re.I | re.S,
        ),
    ),
]


def evidence_tail(text: str) -> str:
    text = text.strip()
    return text[-800:]


def first_int(pattern: str, text: str) -> int | None:
    match = re.search(pattern, text, re.I | re.S)
    if not match:
        return None
    try:
        return int(match.group(1))
    except ValueError:
        return None


def memory_mib(value: str | None) -> int | None:
    if not value:
        return None
    match = re.match(r"([0-9.]+)\s*(GiB|MiB|GB|MB)", value.strip(), re.I)
    if not match:
        return None
    amount = float(match.group(1))
    unit = match.group(2).lower()
    if unit in {"gib", "gb"}:
        return int(amount * 1024 + 0.999)
    return int(amount + 0.999)


def detail_for(pattern: str, text: str, match: re.Match[str]) -> dict[str, object]:
    details: dict[str, object] = {}
    if pattern == "gpu_oom":
        requested_match = re.search(
            r"Tried to allocate\s+([0-9.]+\s*(?:GiB|MiB|GB|MB))", text, re.I
        )
        requested = memory_mib(requested_match.group(1) if requested_match else None)
        if requested:
            details["requested_mib"] = requested
        device_id = first_int(r"(?:GPU|CUDA device)\s+(\d+)", text)
        if device_id is not None:
            details["cuda_device_id"] = device_id
    elif pattern == "cuda_error":
        code = first_int(r"CUDA(?: error)?:\s*(\d+)", text)
        if code is not None:
            details["cuda_error_code"] = code
        err_str_match = re.search(
            r"(CUDA_ERROR_[A-Z0-9_]+|CUDNN_STATUS_[A-Z0-9_]+|cuBLAS error[^.\n]*|cuFFT error[^.\n]*)",
            text,
            re.I,
        )
        if err_str_match:
            details["cuda_error_str"] = err_str_match.group(1).strip()
        kernel = re.search(r"kernel(?: name)?[:=]\s*([A-Za-z0-9_.$-]+)", text, re.I)
        if kernel:
            details["kernel_name"] = kernel.group(1)
    elif pattern == "module_not_found" and match.lastindex:
        if match.group(1):
            details["missing_module"] = match.group(1)
    elif pattern == "timeout":
        budget = first_int(
            r"(?:budget|max(?:imum)? time|timeout)\D+(\d+)\s*(?:seconds|secs|s)\b", text
        )
        if budget:
            details["budget_seconds"] = budget
        elapsed = first_int(r"elapsed\D+(\d+)\s*(?:seconds|secs|s)\b", text)
        if elapsed:
            details["elapsed_seconds"] = elapsed
        details["enforcer"] = (
            "provider"
            if "provider" in text.lower()
            else "wrapper"
            if "wrapper" in text.lower()
            else "agent"
        )
    elif pattern == "preempted":
        provider = re.search(r"\b(vastai|runpod|aws|gcp|azure|fly)\b", text, re.I)
        if provider:
            details["provider"] = provider.group(1).lower()
        notice = first_int(
            r"(\d+)\s*(?:seconds|secs|s).*?(?:preempt|terminat|interrupt)", text
        )
        if notice:
            details["notice_seconds_before_termination"] = notice
    return details


def diagnose(text: str, now: int) -> dict[str, object]:
    for pattern, category, message, confidence, regex in RULES:
        match = regex.search(text)
        if not match:
            continue
        return {
            "pattern": pattern,
            "category": category,
            "message": message,
            "detected_by": "backfill",
            "detected_at": now,
            "confidence": confidence,
            "details": detail_for(pattern, text, match),
            "matched_text": match.group(0),
            "evidence_tail_chars": len(evidence_tail(text)),
        }
    tail = evidence_tail(text)
    return {
        "pattern": "unknown",
        "category": "unknown",
        "message": "Unknown failure",
        "detected_by": "backfill",
        "detected_at": now,
        "confidence": 0.1,
        "details": {},
        "matched_text": tail,
        "evidence_tail_chars": len(tail),
    }


def ensure_column(conn: sqlite3.Connection) -> None:
    columns = {row[1] for row in conn.execute("PRAGMA table_info(job_attempts)")}
    if "error_diagnosis_backfilled" not in columns:
        conn.execute(
            "ALTER TABLE job_attempts ADD COLUMN error_diagnosis_backfilled INTEGER NOT NULL DEFAULT 0"
        )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--db", default=str(Path.home() / ".config/weft/jobs.db"))
    parser.add_argument("--limit", type=int, default=0)
    parser.add_argument("--overwrite", action="store_true")
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()

    conn = sqlite3.connect(args.db)
    if not args.dry_run:
        ensure_column(conn)
    where = ["status = 'failed'", "TRIM(COALESCE(error_message, '')) != ''"]
    if not args.overwrite:
        where.append("TRIM(COALESCE(error_diagnosis, '')) = ''")
    query = (
        "SELECT id, error_message FROM job_attempts WHERE "
        + " AND ".join(where)
        + " ORDER BY id"
        + (" LIMIT ?" if args.limit > 0 else "")
    )
    params = (args.limit,) if args.limit > 0 else ()

    now = int(time.time())
    counts: dict[str, int] = {}
    updated = 0
    for attempt_id, error_message in conn.execute(query, params):
        diagnosis = diagnose(error_message, now)
        pattern = str(diagnosis["pattern"])
        counts[pattern] = counts.get(pattern, 0) + 1
        updated += 1
        if not args.dry_run:
            conn.execute(
                "UPDATE job_attempts SET error_diagnosis = ?, error_diagnosis_backfilled = 1 WHERE id = ?",
                (json.dumps(diagnosis, sort_keys=True), attempt_id),
            )
    if not args.dry_run:
        conn.commit()
    print(f"{'would update' if args.dry_run else 'updated'} {updated} attempts")
    for pattern, count in sorted(counts.items()):
        print(f"{pattern}\t{count}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
