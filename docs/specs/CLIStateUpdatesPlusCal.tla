------------------------------ MODULE CLIStateUpdatesPlusCal ------------------------------
EXTENDS TLC, Sequences

\* This model focuses on how CLI commands update the local DB rows and the
\* pending-ops queue. It abstracts away remote IO, timestamps, and validation.

Draft == "draft"
Queued == "queued"
Starting == "starting"
Running == "running"
Completed == "completed"
Dead == "dead"
Failed == "failed"
Killed == "killed"
Canceled == "canceled"
NoPending == "no-pending"
NoTarget == "no-target"

StatusSet == {Draft, Queued, Starting, Running, Completed, Dead, Failed, Killed, Canceled}
PendingSet == StatusSet \cup {NoPending}
TargetSet == StatusSet \cup {NoTarget}
OpKindSet == {"queue", "start", "kill", "draft", "update", "restart", "cancel"}

JobID == Nat

OpRecord == [job: JobID, kind: OpKindSet, target: TargetSet]
JobRecord == [status: StatusSet, base: StatusSet, pending: PendingSet]

(* --algorithm CliUpdates
variables
    jobs \in [JobID -> JobRecord],
    nextID \in Nat,
    opsQueue \in Seq(OpRecord);

define
    HasJob(j) == j \in DOMAIN jobs
    PendingActive(j) == jobs[j].pending # NoPending
end define;

procedure EnqueueOp(jobId, kind, target)
begin
    EnqueueOpDo:
        opsQueue := Append(opsQueue, [job |-> jobId, kind |-> kind, target |-> target]);
    EnqueueOpReturn:
        return;
end procedure;

begin
Init:
    jobs := [j \in {} |-> [status |-> Draft, base |-> Draft, pending |-> NoPending]];
    nextID := 1;
    opsQueue := << >>;

Loop:
    while TRUE do
        either
            RunImmediate:
                \* remote-jobs run <host> <cmd>
                jobs[nextID] := [status |-> Starting, base |-> Draft, pending |-> Running];
                call EnqueueOp(nextID, "start", Running);
                nextID := nextID + 1;
        or
            QueueAdd:
                \* remote-jobs queue add <host> <cmd>
                jobs[nextID] := [status |-> Queued, base |-> Draft, pending |-> Queued];
                call EnqueueOp(nextID, "queue", Queued);
                nextID := nextID + 1;
        or
            DraftJob:
                \* remote-jobs draft <job>
                with j \in DOMAIN jobs do
                    jobs[j] := [status |-> Draft, base |-> jobs[j].base, pending |-> Draft];
                    call EnqueueOp(j, "draft", Draft);
                end with;
        or
            KillJob:
                \* remote-jobs kill <job>
                with j \in DOMAIN jobs do
                    jobs[j] := [status |-> jobs[j].status, base |-> jobs[j].base, pending |-> Killed];
                    call EnqueueOp(j, "kill", Killed);
                end with;
        or
            RestartJob:
                \* remote-jobs restart <job>
                with j \in DOMAIN jobs do
                    jobs[j] := [status |-> Queued, base |-> jobs[j].base, pending |-> Queued];
                    call EnqueueOp(j, "restart", Queued);
                end with;
        or
            CancelQueued:
                \* remote-jobs cancel <job> (queued only)
                with j \in DOMAIN jobs do
                    jobs[j] := [status |-> Queued, base |-> jobs[j].base, pending |-> Canceled];
                    call EnqueueOp(j, "cancel", Canceled);
                end with;
        or
            UpdateQueued:
                \* remote-jobs describe/edit <job> (queued only)
                \* Operational changes (dir/env/command/status) enqueue a remote update.
                with j \in DOMAIN jobs do
                    with updateKind \in {"metadata", "operational"} do
                        jobs[j] := [status |-> jobs[j].status, base |-> jobs[j].base, pending |-> jobs[j].pending];
                        if updateKind = "operational" then
                            call EnqueueOp(j, "update", Queued);
                        end if;
                    end with;
                end with;
        or
            SyncApplied:
                \* Successful sync updates base to the current status.
                with j \in DOMAIN jobs do
                    if jobs[j].pending = NoPending then
                        jobs[j] := [status |-> jobs[j].status, base |-> jobs[j].status, pending |-> jobs[j].pending];
                    end if;
                end with;
        end either;
    end while;
end algorithm; *)

TypeInvariant ==
    /\ jobs \in [JobID -> JobRecord]
    /\ nextID \in Nat
    /\ opsQueue \in Seq(OpRecord)

=============================================================================
