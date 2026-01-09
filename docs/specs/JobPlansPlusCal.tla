------------------------------ MODULE JobPlansPlusCal ------------------------------
EXTENDS TLC, Sequences

Draft == "draft"
Queued == "queued"
Running == "running"
NoPending == "no-pending"

StatusSet == {Draft, Queued, Running}
PendingSet == StatusSet \cup {NoPending}
BlockSet == {"parallel", "series"}
WaitSet == {"success", "any"}
DependencySet == {"after", "after-any"}

JobID == Nat

JobRecord == [status: StatusSet, pending: PendingSet, block: BlockSet]

(* --algorithm JobPlans
variables
    jobs \in [JobID -> JobRecord],
    nextID \in Nat,
    planWait \in WaitSet,
    dependencyMode \in DependencySet;

begin
Init:
    jobs := [j \in {} |-> [status |-> Draft, pending |-> NoPending, block |-> "parallel"]];
    nextID := 1;
    planWait := "success";
    dependencyMode := "after";

Loop:
    while TRUE do
        either
            SubmitParallelJob:
                jobs[nextID] := [status |-> Running, pending |-> Running, block |-> "parallel"];
                nextID := nextID + 1;
        or
            SubmitSeriesJob:
                jobs[nextID] := [status |-> Queued, pending |-> Queued, block |-> "series"];
                nextID := nextID + 1;
        or
            SetSeriesWait:
                with w \in WaitSet do
                    planWait := w;
                    if w = "success" then
                        dependencyMode := "after";
                    else
                        dependencyMode := "after-any";
                    end if;
                end with;
        end either;
    end while;
end algorithm; *)

TypeInvariant ==
    /\ jobs \in [JobID -> JobRecord]
    /\ nextID \in Nat
    /\ planWait \in WaitSet
    /\ dependencyMode \in DependencySet

=============================================================================
