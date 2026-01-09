------------------------------ MODULE JobPlansPlusCal ------------------------------
EXTENDS TLC, Sequences, Naturals

Draft == "draft"
Queued == "queued"
Running == "running"
NoPending == "no-pending"

StatusSet == {Draft, Queued, Running}
PendingSet == StatusSet \cup {NoPending}
BlockSet == {"parallel", "series"}
WaitSet == {"success", "any"}
DependencySet == {"after", "after-any"}

\* Bound for model checking - set in .cfg file
CONSTANT MaxJobs
JobID == 1..MaxJobs

JobRecord == [status: StatusSet, pending: PendingSet, block: BlockSet]

(* --algorithm JobPlans
variables
    jobs = [j \in {} |-> [status |-> Draft, pending |-> NoPending, block |-> "parallel"]],
    nextID = 1,
    planWait = "success",
    dependencyMode = "after";

begin
InitState:
    jobs := [j \in {} |-> [status |-> Draft, pending |-> NoPending, block |-> "parallel"]];
    nextID := 1;
    planWait := "success";
    dependencyMode := "after";

Loop:
    while TRUE do
        either
            SubmitParallelJob:
                await nextID <= MaxJobs;
                jobs := jobs @@ (nextID :> [status |-> Running, pending |-> Running, block |-> "parallel"]);
                nextID := nextID + 1;
        or
            SubmitSeriesJob:
                await nextID <= MaxJobs;
                jobs := jobs @@ (nextID :> [status |-> Queued, pending |-> Queued, block |-> "series"]);
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
\* BEGIN TRANSLATION (chksum(pcal) = "d90e2d7" /\ chksum(tla) = "98c3a864")
VARIABLES jobs, nextID, planWait, dependencyMode, pc

vars == << jobs, nextID, planWait, dependencyMode, pc >>

Init == (* Global variables *)
        /\ jobs = [j \in {} |-> [status |-> Draft, pending |-> NoPending, block |-> "parallel"]]
        /\ nextID = 1
        /\ planWait = "success"
        /\ dependencyMode = "after"
        /\ pc = "InitState"

InitState == /\ pc = "InitState"
             /\ jobs' = [j \in {} |-> [status |-> Draft, pending |-> NoPending, block |-> "parallel"]]
             /\ nextID' = 1
             /\ planWait' = "success"
             /\ dependencyMode' = "after"
             /\ pc' = "Loop"

Loop == /\ pc = "Loop"
        /\ \/ /\ pc' = "SubmitParallelJob"
           \/ /\ pc' = "SubmitSeriesJob"
           \/ /\ pc' = "SetSeriesWait"
        /\ UNCHANGED << jobs, nextID, planWait, dependencyMode >>

SubmitParallelJob == /\ pc = "SubmitParallelJob"
                     /\ nextID <= MaxJobs
                     /\ jobs' = jobs @@ (nextID :> [status |-> Running, pending |-> Running, block |-> "parallel"])
                     /\ nextID' = nextID + 1
                     /\ pc' = "Loop"
                     /\ UNCHANGED << planWait, dependencyMode >>

SubmitSeriesJob == /\ pc = "SubmitSeriesJob"
                   /\ nextID <= MaxJobs
                   /\ jobs' = jobs @@ (nextID :> [status |-> Queued, pending |-> Queued, block |-> "series"])
                   /\ nextID' = nextID + 1
                   /\ pc' = "Loop"
                   /\ UNCHANGED << planWait, dependencyMode >>

SetSeriesWait == /\ pc = "SetSeriesWait"
                 /\ \E w \in WaitSet:
                      /\ planWait' = w
                      /\ IF w = "success"
                            THEN /\ dependencyMode' = "after"
                            ELSE /\ dependencyMode' = "after-any"
                 /\ pc' = "Loop"
                 /\ UNCHANGED << jobs, nextID >>

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == pc = "Done" /\ UNCHANGED vars

Next == InitState \/ Loop \/ SubmitParallelJob \/ SubmitSeriesJob
           \/ SetSeriesWait
           \/ Terminating

Spec == Init /\ [][Next]_vars

Termination == <>(pc = "Done")

\* END TRANSLATION 

\* State constraint for bounded model checking
StateConstraint ==
    /\ nextID <= MaxJobs + 1

TypeInvariant ==
    /\ DOMAIN jobs \subseteq JobID
    /\ \A j \in DOMAIN jobs: jobs[j] \in JobRecord
    /\ nextID \in 1..(MaxJobs + 1)
    /\ planWait \in WaitSet
    /\ dependencyMode \in DependencySet

=============================================================================
