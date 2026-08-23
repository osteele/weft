# Host-local RAM admission

The inventory queue runner prevents a concurrent set of jobs from committing
more than 90% of host RAM. Placement still answers whether one job can ever fit
the host; the runner answers whether it can start now.

Each queue payload carries a reservation equal to the larger of:

- the effective `--cpu-mem` requirement, including its normal safety headroom;
- the predictor's peak-RSS upper confidence bound, when available.

At dispatch, the runner reads live host memory and current RSS for each running
job. Live usage already includes resident job pages and unrelated processes, so
the projected total is:

```text
live used
+ sum(max(running reservation - running current RSS, 0))
+ candidate reservation
```

This closes the interval between process start and model residency without
double-counting pages that are already resident. Reservations are derived from
queue payloads and running state, not acquired and released in a separate
ledger. A runner restart therefore reconstructs the same commitments.

RAM-blocked jobs remain at the head of the FIFO queue and are reconsidered as
usage changes or jobs finish. If the host memory probe is unavailable, this
gate fails open so an older or unusual host is not permanently wedged.
