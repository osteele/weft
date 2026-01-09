--------------------------- MODULE NetworkResiliencePlusCal ---------------------------
EXTENDS TLC, Sequences

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

HostSet == {"hostA", "hostB"}
JobID == Nat

JobRecord == [status: StatusSet, pending: PendingSet, host: HostSet]

(* --algorithm NetworkResilience
variables
    jobs \in [JobID -> JobRecord],
    nextID \in Nat,
    hostUp \in [HostSet -> BOOLEAN];

procedure RecordIntent(jobId, target)
begin
    RecordIntentDo:
        jobs[jobId] := [status |-> target, pending |-> target, host |-> jobs[jobId].host];
    RecordIntentReturn:
        return;
end procedure;

begin
Init:
    jobs := [j \in {} |-> [status |-> Draft, pending |-> NoPending, host |-> "hostA"]];
    nextID := 1;
    hostUp := [h \in HostSet |-> TRUE];

Loop:
    while TRUE do
        either
            SubmitWhileOffline:
                with h \in HostSet do
                    hostUp[h] := FALSE;
                    jobs[nextID] := [status |-> Queued, pending |-> Queued, host |-> h];
                    nextID := nextID + 1;
                end with;
        or
            SyncWhenOnline:
                with j \in DOMAIN jobs do
                    if hostUp[jobs[j].host] /\ jobs[j].pending # NoPending then
                        jobs[j] := [status |-> jobs[j].pending, pending |-> NoPending, host |-> jobs[j].host];
                    end if;
                end with;
        or
            HostFlap:
                with h \in HostSet do
                    hostUp[h] := ~hostUp[h];
                end with;
        end either;
    end while;
end algorithm; *)
\* BEGIN TRANSLATION (chksum(pcal) = "74736ea1" /\ chksum(tla) = "7310f177")
CONSTANT defaultInitValue
VARIABLES jobs, nextID, hostUp, pc, stack, jobId, target

vars == << jobs, nextID, hostUp, pc, stack, jobId, target >>

Init == (* Global variables *)
        /\ jobs \in [JobID -> JobRecord]
        /\ nextID \in Nat
        /\ hostUp \in [HostSet -> BOOLEAN]
        (* Procedure RecordIntent *)
        /\ jobId = defaultInitValue
        /\ target = defaultInitValue
        /\ stack = << >>
        /\ pc = "Init"

RecordIntentDo == /\ pc = "RecordIntentDo"
                  /\ jobs' = [jobs EXCEPT ![jobId] = [status |-> target, pending |-> target, host |-> jobs[jobId].host]]
                  /\ pc' = "RecordIntentReturn"
                  /\ UNCHANGED << nextID, hostUp, stack, jobId, target >>

RecordIntentReturn == /\ pc = "RecordIntentReturn"
                      /\ pc' = Head(stack).pc
                      /\ jobId' = Head(stack).jobId
                      /\ target' = Head(stack).target
                      /\ stack' = Tail(stack)
                      /\ UNCHANGED << jobs, nextID, hostUp >>

RecordIntent == RecordIntentDo \/ RecordIntentReturn

Init == /\ pc = "Init"
        /\ jobs' = [j \in {} |-> [status |-> Draft, pending |-> NoPending, host |-> "hostA"]]
        /\ nextID' = 1
        /\ hostUp' = [h \in HostSet |-> TRUE]
        /\ pc' = "Loop"
        /\ UNCHANGED << stack, jobId, target >>

Loop == /\ pc = "Loop"
        /\ \/ /\ pc' = "SubmitWhileOffline"
           \/ /\ pc' = "SyncWhenOnline"
           \/ /\ pc' = "HostFlap"
        /\ UNCHANGED << jobs, nextID, hostUp, stack, jobId, target >>

SubmitWhileOffline == /\ pc = "SubmitWhileOffline"
                      /\ \E h \in HostSet:
                           /\ hostUp' = [hostUp EXCEPT ![h] = FALSE]
                           /\ jobs' = [jobs EXCEPT ![nextID] = [status |-> Queued, pending |-> Queued, host |-> h]]
                           /\ nextID' = nextID + 1
                      /\ pc' = "Loop"
                      /\ UNCHANGED << stack, jobId, target >>

SyncWhenOnline == /\ pc = "SyncWhenOnline"
                  /\ \E j \in DOMAIN jobs:
                       IF hostUp[jobs[j].host] /\ jobs[j].pending # NoPending
                          THEN /\ jobs' = [jobs EXCEPT ![j] = [status |-> jobs[j].pending, pending |-> NoPending, host |-> jobs[j].host]]
                          ELSE /\ TRUE
                               /\ jobs' = jobs
                  /\ pc' = "Loop"
                  /\ UNCHANGED << nextID, hostUp, stack, jobId, target >>

HostFlap == /\ pc = "HostFlap"
            /\ \E h \in HostSet:
                 hostUp' = [hostUp EXCEPT ![h] = ~hostUp[h]]
            /\ pc' = "Loop"
            /\ UNCHANGED << jobs, nextID, stack, jobId, target >>

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == pc = "Done" /\ UNCHANGED vars

Next == RecordIntent \/ Init \/ Loop \/ SubmitWhileOffline
           \/ SyncWhenOnline \/ HostFlap
           \/ Terminating

Spec == Init /\ [][Next]_vars

Termination == <>(pc = "Done")

\* END TRANSLATION 

TypeInvariant ==
    /\ jobs \in [JobID -> JobRecord]
    /\ nextID \in Nat
    /\ hostUp \in [HostSet -> BOOLEAN]

=============================================================================
