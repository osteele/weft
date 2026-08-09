# /// script
# requires-python = ">=3.10"
# [tool.weft]
# isolated = true
# ///
"""Record when a benchmark-isolation follower passes the publication barrier."""

from __future__ import annotations

import json
import os
import signal
import time
from pathlib import Path


def handle_sigterm(_signum: int, _frame: object) -> None:
    print("[signal] received SIGTERM", flush=True)
    raise SystemExit(143)


def main() -> None:
    signal.signal(signal.SIGTERM, handle_sigterm)
    started = time.time()
    output = Path("output/")
    output.mkdir(parents=True, exist_ok=True)
    result = {
        "benchmark_job_id": os.environ.get("WEFT_JOB_ID", "unknown"),
        "started_at_unix": started,
    }
    (output / "benchmark.json").write_text(json.dumps(result, indent=2) + "\n")
    print(f"[RESULT] benchmark barrier released started_at_unix={started:.6f}", flush=True)


if __name__ == "__main__":
    main()
