------------------------------ MODULE JobMetadataInvariantsPlusCal ------------------------------
EXTENDS TLC

\* This spec models the local metadata invariants that must hold when a job
\* transitions back to queued or draft.

VARIABLE
  \* @type: Str;
  status,
  \* @type: Bool;
  session,
  \* @type: Bool;
  started

Statuses == {"queued", "draft", "starting", "running", "completed", "dead", "failed", "killed", "canceled"}

Init ==
    /\ status = "queued"
    /\ session = FALSE
    /\ started = FALSE

StartQueueRunner ==
    /\ status = "queued"
    /\ status' = "running"
    /\ session' = FALSE
    /\ started' = TRUE

StartTmux ==
    /\ status = "queued"
    /\ status' = "running"
    /\ session' = TRUE
    /\ started' = TRUE

Requeue ==
    /\ status \in {"running", "starting", "draft"}
    /\ status' = "queued"
    /\ session' = FALSE
    /\ started' = FALSE

Draft ==
    /\ status # "draft"
    /\ status' = "draft"
    /\ session' = FALSE
    /\ started' = FALSE

Complete ==
    /\ status = "running"
    /\ status' = "completed"
    /\ session' = FALSE
    /\ started' = started

Next ==
    \/ StartQueueRunner
    \/ StartTmux
    \/ Requeue
    \/ Draft
    \/ Complete

Spec == Init /\ [][Next]_<<status, session, started>>

QueuedOrDraftClearsMetadata ==
    (status = "queued" \/ status = "draft") => ~session /\ ~started

SessionImpliesRunning ==
    session => status = "running"

=============================================================================
