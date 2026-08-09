# /// script
# requires-python = ">=3.10"
# [tool.weft]
# isolated = true
# ///
"""Validate a dependency-critical artifact staged by --needs."""

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


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--token", default="exp-023-real-campaign")
    args = parser.parse_args()
    signal.signal(signal.SIGTERM, handle_sigterm)

    source = Path("output/critical.json")
    if not source.is_file():
        raise SystemExit(f"required artifact was not staged: {source}")
    critical = json.loads(source.read_text())
    if critical.get("token") != args.token:
        raise SystemExit(f"unexpected artifact token: {critical.get('token')!r}")

    output = Path("output/")
    output.mkdir(parents=True, exist_ok=True)
    result = {
        "consumer_job_id": os.environ.get("WEFT_JOB_ID", "unknown"),
        "producer_job_id": critical.get("producer_job_id"),
        "token": critical["token"],
        "validated_at_unix": time.time(),
    }
    (output / "consumer.json").write_text(json.dumps(result, indent=2) + "\n")
    print(
        f"[RESULT] consumer validated token={result['token']} producer_job_id={result['producer_job_id']}",
        flush=True,
    )


if __name__ == "__main__":
    main()
