# /// script
# requires-python = ">=3.10"
# [tool.weft]
# isolated = true
# ///
"""Create a small dependency-critical artifact and unrelated bulk output."""

from __future__ import annotations

import argparse
import json
import os
import signal
import time
from pathlib import Path


def handle_sigterm(_signum: int, _frame: object) -> None:
    print("[signal] received SIGTERM", flush=True)
    raise SystemExit(143)


def write_json_atomic(path: Path, value: object) -> None:
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, indent=2) + "\n")
    temporary.replace(path)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--bulk-mib", type=int, default=1024)
    parser.add_argument("--token", default="exp-023-real-campaign")
    args = parser.parse_args()
    if args.bulk_mib <= 0:
        raise SystemExit("--bulk-mib must be positive")

    signal.signal(signal.SIGTERM, handle_sigterm)
    output = Path("output/")
    output.mkdir(parents=True, exist_ok=True)
    started = time.time()
    critical = {
        "token": args.token,
        "producer_job_id": os.environ.get("WEFT_JOB_ID", "unknown"),
        "created_at_unix": started,
    }
    write_json_atomic(output / "critical.json", critical)
    print(f"[RESULT] critical artifact written token={args.token}", flush=True)

    chunk = bytes(8 * 1024 * 1024)
    chunks = args.bulk_mib // 8
    remainder = args.bulk_mib % 8
    with (output / "bulk.bin").open("wb") as stream:
        for index in range(chunks):
            stream.write(chunk)
            if (index + 1) % 8 == 0:
                print(f"Progress: {(index + 1) * 8}/{args.bulk_mib} MiB", flush=True)
        if remainder:
            stream.write(bytes(remainder * 1024 * 1024))
        stream.flush()
        os.fsync(stream.fileno())

    completed = time.time()
    summary = {
        "bulk_bytes": args.bulk_mib * 1024 * 1024,
        "critical": critical,
        "started_at_unix": started,
        "completed_at_unix": completed,
    }
    write_json_atomic(output / "producer.json", summary)
    if (output / "bulk.bin").stat().st_size != summary["bulk_bytes"]:
        raise SystemExit("bulk output size mismatch")
    print(
        f"[RESULT] producer complete bulk_bytes={summary['bulk_bytes']} duration_s={completed - started:.3f}",
        flush=True,
    )


if __name__ == "__main__":
    main()
