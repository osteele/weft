---
status: accepted
date: 2026-08-18
---

# 9. Read the container before declaring it dead

## Context and Problem Statement

Every pre-bootstrap signal the reconciler weighs — the OnStart probe, the
OnStart stage marker, the bootstrap stage, the heartbeat, the instance phase —
is an R2 read of a *push* mechanism. Each of those markers exists only because
weft's OnStart script ran and wrote it. The watchdog is therefore a listener:
it infers the container's condition from what the container says about itself.

That works until the thing that fails is the script. When a provider takes
weft's `--onstart-cmd` and does not install it, every marker is absent at
once — and absence in this design is irreducibly ambiguous. It means the
script never ran, or the container has no outbound network, or R2 is
unreachable, or this machine's own network is down. `weft instance diagnose`
can only hedge accordingly: *"container probably never executed OnStart, or
had no outbound network."*

Rule 4d is built around that ambiguity and is right to be. It conjoins six
absence signals, each obliged to report its safe value when the underlying
read fails, and only fires after a window. The conjunction is what keeps a
flaky R2 read from destroying a healthy rental.

The cost is that a genuinely dead container is indistinguishable from an
unobservable one for the whole window. Instance wi7309 (2026-08-18) sat 86
minutes and $3.80 in exactly that state: vast.ai stored the script in its
instance record, injected `extra_env` into the container, and never wrote the
script to disk. Its `/root/onstart.sh` was the provider's own 82-byte stub.
The container was SSH-reachable the entire time, and one command would have
settled it.

Waiting longer cannot resolve an ambiguity; only a second, independent
observation can.

## Decision Outcome

**Add a pull-based observation channel: when the probe is confirmed absent
inside the dud window, SSH into the container and read
`/root/onstart.sh`.** A script without weft's sentinel is positive evidence
that the provider never installed it, and rule 4c-onstart terminates on that
evidence with no window at all.

The verdict is a three-state `cloud.OnStartVerification`. Only
`ConfirmedMissing` is actionable. Absent SSH details, an unreachable host, a
timeout, and an unreadable file are all `Unknown`, and `Unknown` falls through
to rule 4d's timeout unchanged. This keeps the project's standing rule that a
destructive action requires positive evidence: the new channel can only ever
*add* a confirmed negative, never convert silence into one.

Matching is on the absence of weft's own sentinel (`_weft_stage`, the function
every stage write funnels through), not on any provider's placeholder text.
Providers substitute their own stub when no start script is installed and that
text is theirs to change; what weft sent is the only stable referent. The
coupling is load-bearing in the dangerous direction — a rename that removed
the sentinel from `DefaultOnStartCmd` without updating the constant would
confirm-missing every healthy container in the fleet — so a test asserts the
sentinel is a substring of both OnStart command builders.

The check is gated to instances already suspected: inside the dud window, only
after the probe is confirmed absent, only before a job has started, and only
after a two-minute grace. A healthy instance whose probe has landed is never
reached at over SSH; a healthy one that is merely slow to probe is reached
once, answers `ConfirmedInstalled`, and is not asked again.

It is also gated to vast.ai. The premise — sentinel absent from
`/root/onstart.sh` implies weft's script was not installed — holds only for a
provider that takes an OnStart command. RunPod rejects per-pod startup
commands and bootstraps through its template's `dockerStartCmd`, so a
base-image start script there would read as confirmed-missing on a healthy
pod, and this verdict destroys instances with no window.

### Consequences

- The reconciler now performs network I/O against instances during its check
  pass. It is hard-bounded (one attempt, `ConnectTimeout` plus a killed
  process at five seconds, only for probe-absent instances inside the dud
  window) but it is a new failure surface in a loop that was previously
  R2-only. The bound has to stay under the fast cloud-sync budget, or one
  unreachable instance makes `weft list` report a degraded sync.
- The verdict is memoized in process, not persisted. `ConfirmedInstalled` is
  settled and never re-asked; `Unknown` is re-asked at most once a minute. A
  daemon restart re-asks from scratch, which costs one round trip.
- Two rules can now terminate the same instance for the same underlying
  fault, on different evidence and different timescales. 4c-onstart fires in
  seconds on a positive reading; 4d still fires on the timeout when SSH is
  unavailable. A reader who removes 4d as redundant would reintroduce the
  wi7309 bleed for every unreachable container.
- The verification deliberately does not retry. Re-reading a container's
  filesystem four times in fifteen seconds cannot learn anything, and the
  next reconcile pass asks again anyway — so `RunOnInstanceOnce`, not
  `RunOnInstance`, is what this path calls.
- SSH reachability is now load-bearing for a termination decision on vast.ai
  rentals. Other providers, and network configurations without direct SSH,
  get the old timeout behaviour and nothing worse.

## Considered Options

### Compare the provider's echoed OnStart field against what weft sent

Rejected: it does not detect this failure. vast.ai's API returned weft's full
2318-byte script correctly for wi7309 while the container had none. The
provider's record and the container's filesystem are separate pieces of state
and it was the second that was wrong, so only reading the container catches it.

### Widen or lengthen the absence conjunction

Rejected: it treats a resolution problem as a confidence problem. No number of
additional absent R2 markers distinguishes "the script never ran" from "we
cannot see R2", because every one of them has the same single cause upstream.
More absence signals would make rule 4d slower to fire, not more accurate.

### Have the agent report installation

Rejected as circular. Any report weft could add would be written by the very
script whose installation is in question.

### Verify at launch for every instance

Rejected: it puts an SSH round trip on the critical path of every launch to
catch a rare provider fault, and it races container startup. Gating on a
confirmed-absent probe reaches only instances that are already anomalous,
which is a small fraction of launches and exactly the right ones.

## More Information

- **Builds on**: [0008](0008-cap-the-borrowed-curve-in-dud-detection.md)
- **References**: `specs/campaign-lifecycle.allium` § OnStartScriptMissing and
  § DudVastDetection; `internal/cloud/onstart_verify.go`; incident instance
  wi7309
