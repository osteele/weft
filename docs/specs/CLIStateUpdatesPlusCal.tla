------------------------------ MODULE CLIStateUpdatesPlusCal ------------------------------
EXTENDS TLC, Sequences, Naturals

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

\* Bound for model checking - set in .cfg file
CONSTANT MaxJobs
JobID == 1..MaxJobs
MaxQueueLen == MaxJobs * 2

OpRecord == [job: JobID, kind: OpKindSet, target: TargetSet]
JobRecord == [status: StatusSet, base: StatusSet, pending: PendingSet]

(* --algorithm CliUpdates
variables
    jobs = [j \in {} |-> [status |-> Draft, base |-> Draft, pending |-> NoPending]],
    nextID = 1,
    opsQueue = << >>;

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
InitState:
    jobs := [j \in {} |-> [status |-> Draft, base |-> Draft, pending |-> NoPending]];
    nextID := 1;
    opsQueue := << >>;

Loop:
    while TRUE do
        either
            RunImmediate:
                \* remote-jobs run <host> <cmd>
                await nextID <= MaxJobs;
                jobs := jobs @@ (nextID :> [status |-> Starting, base |-> Draft, pending |-> Running]);
                call EnqueueOp(nextID, "start", Running);
            RunImmediatePost:
                nextID := nextID + 1;
        or
            QueueAdd:
                \* remote-jobs queue add <host> <cmd>
                await nextID <= MaxJobs;
                jobs := jobs @@ (nextID :> [status |-> Queued, base |-> Draft, pending |-> Queued]);
                call EnqueueOp(nextID, "queue", Queued);
            QueueAddPost:
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
\* BEGIN TRANSLATION (chksum(pcal) = "86363293" /\ chksum(tla) = "6f32c7d4")
CONSTANT defaultInitValue
VARIABLES jobs, nextID, opsQueue, pc, stack

(* define statement *)
HasJob(j) == j \in DOMAIN jobs
PendingActive(j) == jobs[j].pending # NoPending

VARIABLES jobId, kind, target

vars == << jobs, nextID, opsQueue, pc, stack, jobId, kind, target >>

Init == (* Global variables *)
        /\ jobs = [j \in {} |-> [status |-> Draft, base |-> Draft, pending |-> NoPending]]
        /\ nextID = 1
        /\ opsQueue = << >>
        (* Procedure EnqueueOp *)
        /\ jobId = defaultInitValue
        /\ kind = defaultInitValue
        /\ target = defaultInitValue
        /\ stack = << >>
        /\ pc = "InitState"

EnqueueOpDo == /\ pc = "EnqueueOpDo"
               /\ opsQueue' = Append(opsQueue, [job |-> jobId, kind |-> kind, target |-> target])
               /\ pc' = "EnqueueOpReturn"
               /\ UNCHANGED << jobs, nextID, stack, jobId, kind, target >>

EnqueueOpReturn == /\ pc = "EnqueueOpReturn"
                   /\ pc' = Head(stack).pc
                   /\ jobId' = Head(stack).jobId
                   /\ kind' = Head(stack).kind
                   /\ target' = Head(stack).target
                   /\ stack' = Tail(stack)
                   /\ UNCHANGED << jobs, nextID, opsQueue >>

EnqueueOp == EnqueueOpDo \/ EnqueueOpReturn

InitState == /\ pc = "InitState"
             /\ jobs' = [j \in {} |-> [status |-> Draft, base |-> Draft, pending |-> NoPending]]
             /\ nextID' = 1
             /\ opsQueue' = << >>
             /\ pc' = "Loop"
             /\ UNCHANGED << stack, jobId, kind, target >>

Loop == /\ pc = "Loop"
        /\ \/ /\ pc' = "RunImmediate"
           \/ /\ pc' = "QueueAdd"
           \/ /\ pc' = "DraftJob"
           \/ /\ pc' = "KillJob"
           \/ /\ pc' = "RestartJob"
           \/ /\ pc' = "CancelQueued"
           \/ /\ pc' = "UpdateQueued"
           \/ /\ pc' = "SyncApplied"
        /\ UNCHANGED << jobs, nextID, opsQueue, stack, jobId, kind, target >>

RunImmediate == /\ pc = "RunImmediate"
                /\ nextID <= MaxJobs
                /\ jobs' = jobs @@ (nextID :> [status |-> Starting, base |-> Draft, pending |-> Running])
                /\ /\ jobId' = nextID
                   /\ kind' = "start"
                   /\ stack' = << [ procedure |->  "EnqueueOp",
                                    pc        |->  "RunImmediatePost",
                                    jobId     |->  jobId,
                                    kind      |->  kind,
                                    target    |->  target ] >>
                                \o stack
                   /\ target' = Running
                /\ pc' = "EnqueueOpDo"
                /\ UNCHANGED << nextID, opsQueue >>

RunImmediatePost == /\ pc = "RunImmediatePost"
                    /\ nextID' = nextID + 1
                    /\ pc' = "Loop"
                    /\ UNCHANGED << jobs, opsQueue, stack, jobId, kind, target >>

QueueAdd == /\ pc = "QueueAdd"
            /\ nextID <= MaxJobs
            /\ jobs' = jobs @@ (nextID :> [status |-> Queued, base |-> Draft, pending |-> Queued])
            /\ /\ jobId' = nextID
               /\ kind' = "queue"
               /\ stack' = << [ procedure |->  "EnqueueOp",
                                pc        |->  "QueueAddPost",
                                jobId     |->  jobId,
                                kind      |->  kind,
                                target    |->  target ] >>
                            \o stack
               /\ target' = Queued
            /\ pc' = "EnqueueOpDo"
            /\ UNCHANGED << nextID, opsQueue >>

QueueAddPost == /\ pc = "QueueAddPost"
                /\ nextID' = nextID + 1
                /\ pc' = "Loop"
                /\ UNCHANGED << jobs, opsQueue, stack, jobId, kind, target >>

DraftJob == /\ pc = "DraftJob"
            /\ \E j \in DOMAIN jobs:
                 /\ jobs' = [jobs EXCEPT ![j] = [status |-> Draft, base |-> jobs[j].base, pending |-> Draft]]
                 /\ /\ jobId' = j
                    /\ kind' = "draft"
                    /\ stack' = << [ procedure |->  "EnqueueOp",
                                     pc        |->  "Loop",
                                     jobId     |->  jobId,
                                     kind      |->  kind,
                                     target    |->  target ] >>
                                 \o stack
                    /\ target' = Draft
                 /\ pc' = "EnqueueOpDo"
            /\ UNCHANGED << nextID, opsQueue >>

KillJob == /\ pc = "KillJob"
           /\ \E j \in DOMAIN jobs:
                /\ jobs' = [jobs EXCEPT ![j] = [status |-> jobs[j].status, base |-> jobs[j].base, pending |-> Killed]]
                /\ /\ jobId' = j
                   /\ kind' = "kill"
                   /\ stack' = << [ procedure |->  "EnqueueOp",
                                    pc        |->  "Loop",
                                    jobId     |->  jobId,
                                    kind      |->  kind,
                                    target    |->  target ] >>
                                \o stack
                   /\ target' = Killed
                /\ pc' = "EnqueueOpDo"
           /\ UNCHANGED << nextID, opsQueue >>

RestartJob == /\ pc = "RestartJob"
              /\ \E j \in DOMAIN jobs:
                   /\ jobs' = [jobs EXCEPT ![j] = [status |-> Queued, base |-> jobs[j].base, pending |-> Queued]]
                   /\ /\ jobId' = j
                      /\ kind' = "restart"
                      /\ stack' = << [ procedure |->  "EnqueueOp",
                                       pc        |->  "Loop",
                                       jobId     |->  jobId,
                                       kind      |->  kind,
                                       target    |->  target ] >>
                                   \o stack
                      /\ target' = Queued
                   /\ pc' = "EnqueueOpDo"
              /\ UNCHANGED << nextID, opsQueue >>

CancelQueued == /\ pc = "CancelQueued"
                /\ \E j \in DOMAIN jobs:
                     /\ jobs' = [jobs EXCEPT ![j] = [status |-> Queued, base |-> jobs[j].base, pending |-> Canceled]]
                     /\ /\ jobId' = j
                        /\ kind' = "cancel"
                        /\ stack' = << [ procedure |->  "EnqueueOp",
                                         pc        |->  "Loop",
                                         jobId     |->  jobId,
                                         kind      |->  kind,
                                         target    |->  target ] >>
                                     \o stack
                        /\ target' = Canceled
                     /\ pc' = "EnqueueOpDo"
                /\ UNCHANGED << nextID, opsQueue >>

UpdateQueued == /\ pc = "UpdateQueued"
                /\ \E j \in DOMAIN jobs:
                     \E updateKind \in {"metadata", "operational"}:
                       /\ jobs' = [jobs EXCEPT ![j] = [status |-> jobs[j].status, base |-> jobs[j].base, pending |-> jobs[j].pending]]
                       /\ IF updateKind = "operational"
                             THEN /\ /\ jobId' = j
                                     /\ kind' = "update"
                                     /\ stack' = << [ procedure |->  "EnqueueOp",
                                                      pc        |->  "Loop",
                                                      jobId     |->  jobId,
                                                      kind      |->  kind,
                                                      target    |->  target ] >>
                                                  \o stack
                                     /\ target' = Queued
                                  /\ pc' = "EnqueueOpDo"
                             ELSE /\ pc' = "Loop"
                                  /\ UNCHANGED << stack, jobId, kind, target >>
                /\ UNCHANGED << nextID, opsQueue >>

SyncApplied == /\ pc = "SyncApplied"
               /\ \E j \in DOMAIN jobs:
                    IF jobs[j].pending = NoPending
                       THEN /\ jobs' = [jobs EXCEPT ![j] = [status |-> jobs[j].status, base |-> jobs[j].status, pending |-> jobs[j].pending]]
                       ELSE /\ TRUE
                            /\ jobs' = jobs
               /\ pc' = "Loop"
               /\ UNCHANGED << nextID, opsQueue, stack, jobId, kind, target >>

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == pc = "Done" /\ UNCHANGED vars

Next == EnqueueOp \/ InitState \/ Loop \/ RunImmediate \/ RunImmediatePost
           \/ QueueAdd \/ QueueAddPost \/ DraftJob \/ KillJob \/ RestartJob
           \/ CancelQueued \/ UpdateQueued \/ SyncApplied
           \/ Terminating

Spec == Init /\ [][Next]_vars

Termination == <>(pc = "Done")

\* END TRANSLATION 

\* State constraint for bounded model checking
StateConstraint ==
    /\ nextID <= MaxJobs + 1
    /\ Len(opsQueue) <= MaxQueueLen

TypeInvariant ==
    /\ DOMAIN jobs \subseteq JobID
    /\ \A j \in DOMAIN jobs: jobs[j] \in JobRecord
    /\ nextID \in 1..(MaxJobs + 1)
    /\ opsQueue \in Seq(OpRecord)

=============================================================================
