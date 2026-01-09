------------------------------- MODULE CoreFacadePlusCal -------------------------------
EXTENDS TLC, Sequences

\* Models core vs facade responsibilities and local intent recording.
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

StatusSet == {Draft, Queued, Starting, Running, Completed, Dead, Failed, Killed, Canceled}
PendingSet == StatusSet \cup {NoPending}

RequestSet == {"run", "queue", "kill", "draft", "restart", "update", "cancel"}
HostSet == {"hostA", "hostB"}
JobID == Nat

JobRecord == [status: StatusSet, pending: PendingSet, host: HostSet]

(* --algorithm CoreFacade
variables
    jobs \in [JobID -> JobRecord],
    nextID \in Nat,
    lastRequest \in RequestSet,
    lastJob \in JobID,
    hostUp \in [HostSet -> BOOLEAN];

define
    HasJob(j) == j \in DOMAIN jobs
end define;

procedure RecordIntent(jobId, target)
begin
    RecordIntentDo:
        jobs[jobId] := [status |-> target, pending |-> target, host |-> jobs[jobId].host];
    RecordIntentReturn:
        return;
end procedure;

procedure CreateJob(host, target)
begin
    CreateJobDo:
        jobs[nextID] := [status |-> target, pending |-> target, host |-> host];
        lastJob := nextID;
        nextID := nextID + 1;
    CreateJobReturn:
        return;
end procedure;

begin
Init:
    jobs := [j \in {} |-> [status |-> Draft, pending |-> NoPending, host |-> "hostA"]];
    nextID := 1;
    lastRequest := "run";
    lastJob := 0;
    hostUp := [h \in HostSet |-> TRUE];

Loop:
    while TRUE do
        either
            FacadeRun:
                lastRequest := "run";
                call CreateJob("hostA", Starting);
        or
            FacadeQueue:
                lastRequest := "queue";
                call CreateJob("hostA", Queued);
        or
            FacadeKill:
                lastRequest := "kill";
                with j \in DOMAIN jobs do
                    call RecordIntent(j, Killed);
                end with;
        or
            FacadeDraft:
                lastRequest := "draft";
                with j \in DOMAIN jobs do
                    call RecordIntent(j, Draft);
                end with;
        or
            FacadeRestart:
                lastRequest := "restart";
                with j \in DOMAIN jobs do
                    call RecordIntent(j, Queued);
                end with;
        or
            FacadeCancel:
                lastRequest := "cancel";
                with j \in DOMAIN jobs do
                    call RecordIntent(j, Canceled);
                end with;
        or
            FacadeUpdate:
                lastRequest := "update";
                with j \in DOMAIN jobs do
                    \* Update is a local-only mutation; pending intent unchanged.
                    jobs[j] := [status |-> jobs[j].status, pending |-> jobs[j].pending, host |-> jobs[j].host];
                end with;
        or
            HostFlap:
                with h \in HostSet do
                    hostUp[h] := ~hostUp[h];
                end with;
        end either;
    end while;
end algorithm; *)

TypeInvariant ==
    /\ jobs \in [JobID -> JobRecord]
    /\ nextID \in Nat
    /\ lastRequest \in RequestSet
    /\ lastJob \in JobID
    /\ hostUp \in [HostSet -> BOOLEAN]

=============================================================================
