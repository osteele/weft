--------------------------- MODULE NetworkResiliencePlusCal ---------------------------
EXTENDS TLC, Sequences

Draft == "draft"
Queued == "queued"
Running == "running"
Completed == "completed"
Dead == "dead"
Failed == "failed"
NoPending == "no-pending"

StatusSet == {Draft, Queued, Running, Completed, Dead, Failed}
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

TypeInvariant ==
    /\ jobs \in [JobID -> JobRecord]
    /\ nextID \in Nat
    /\ hostUp \in [HostSet -> BOOLEAN]

=============================================================================
