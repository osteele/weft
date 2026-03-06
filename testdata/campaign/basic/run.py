"""Baseline test: uv sync + GPU detection + output files."""

import json
import os
import sys
import time

import numpy as np
import torch

os.makedirs("output", exist_ok=True)
json.dump(
    {
        "gpu": torch.cuda.get_device_name(0) if torch.cuda.is_available() else "none",
        "torch": torch.__version__,
        "numpy": np.__version__,
        "python": sys.version,
        "ts": time.time(),
    },
    open("output/result.json", "w"),
    indent=2,
)
print("basic: done")
