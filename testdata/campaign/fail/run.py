"""Test job that deliberately fails with a traceback and non-zero exit."""

import sys
import time

print("fail: starting")
time.sleep(2)
print("fail: about to crash", file=sys.stderr)
raise RuntimeError("Deliberate failure for campaign pipeline testing")
