------------------------------- MODULE CoreFacadePlusCal -------------------------------
EXTENDS TLC, Sequences, Naturals

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

\* Bound for model checking - set in .cfg file
CONSTANT MaxJobs
JobID == 1..MaxJobs

JobRecord == [status: StatusSet, pending: PendingSet, host: HostSet]

(* --algorithm CoreFacade
variables
    jobs = [j \in {} |-> [status |-> Draft, pending |-> NoPending, host |-> "hostA"]],
    nextID = 1,
    lastRequest = "run",
    lastJob = 0,
    hostUp = [h \in HostSet |-> TRUE];

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
        await nextID <= MaxJobs;
        jobs := jobs @@ (nextID :> [status |-> target, pending |-> target, host |-> host]);
        lastJob := nextID;
        nextID := nextID + 1;
    CreateJobReturn:
        return;
end procedure;

begin
InitState:
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
\* BEGIN TRANSLATION (chksum(pcal) = "97b66fcb" /\ chksum(tla) = "4cd5cd0f")
\* Parameter target of procedure RecordIntent at line 40 col 31 changed to target_
CONSTANT defaultInitValue
VARIABLES jobs, nextID, lastRequest, lastJob, hostUp, pc, stack

(* define statement *)
HasJob(j) == j \in DOMAIN jobs

VARIABLES jobId, target_, host, target

vars == << jobs, nextID, lastRequest, lastJob, hostUp, pc, stack, jobId, 
           target_, host, target >>

Init == (* Global variables *)
        /\ jobs = [j \in {} |-> [status |-> Draft, pending |-> NoPending, host |-> "hostA"]]
        /\ nextID = 1
        /\ lastRequest = "run"
        /\ lastJob = 0
        /\ hostUp = [h \in HostSet |-> TRUE]
        (* Procedure RecordIntent *)
        /\ jobId = defaultInitValue
        /\ target_ = defaultInitValue
        (* Procedure CreateJob *)
        /\ host = defaultInitValue
        /\ target = defaultInitValue
        /\ stack = << >>
        /\ pc = "InitState"

RecordIntentDo == /\ pc = "RecordIntentDo"
                  /\ jobs' = [jobs EXCEPT ![jobId] = [status |-> target_, pending |-> target_, host |-> jobs[jobId].host]]
                  /\ pc' = "RecordIntentReturn"
                  /\ UNCHANGED << nextID, lastRequest, lastJob, hostUp, stack, 
                                  jobId, target_, host, target >>

RecordIntentReturn == /\ pc = "RecordIntentReturn"
                      /\ pc' = Head(stack).pc
                      /\ jobId' = Head(stack).jobId
                      /\ target_' = Head(stack).target_
                      /\ stack' = Tail(stack)
                      /\ UNCHANGED << jobs, nextID, lastRequest, lastJob, 
                                      hostUp, host, target >>

RecordIntent == RecordIntentDo \/ RecordIntentReturn

CreateJobDo == /\ pc = "CreateJobDo"
               /\ nextID <= MaxJobs
               /\ jobs' = jobs @@ (nextID :> [status |-> target, pending |-> target, host |-> host])
               /\ lastJob' = nextID
               /\ nextID' = nextID + 1
               /\ pc' = "CreateJobReturn"
               /\ UNCHANGED << lastRequest, hostUp, stack, jobId, target_, 
                               host, target >>

CreateJobReturn == /\ pc = "CreateJobReturn"
                   /\ pc' = Head(stack).pc
                   /\ host' = Head(stack).host
                   /\ target' = Head(stack).target
                   /\ stack' = Tail(stack)
                   /\ UNCHANGED << jobs, nextID, lastRequest, lastJob, hostUp, 
                                   jobId, target_ >>

CreateJob == CreateJobDo \/ CreateJobReturn

InitState == /\ pc = "InitState"
             /\ jobs' = [j \in {} |-> [status |-> Draft, pending |-> NoPending, host |-> "hostA"]]
             /\ nextID' = 1
             /\ lastRequest' = "run"
             /\ lastJob' = 0
             /\ hostUp' = [h \in HostSet |-> TRUE]
             /\ pc' = "Loop"
             /\ UNCHANGED << stack, jobId, target_, host, target >>

Loop == /\ pc = "Loop"
        /\ \/ /\ pc' = "FacadeRun"
           \/ /\ pc' = "FacadeQueue"
           \/ /\ pc' = "FacadeKill"
           \/ /\ pc' = "FacadeDraft"
           \/ /\ pc' = "FacadeRestart"
           \/ /\ pc' = "FacadeCancel"
           \/ /\ pc' = "FacadeUpdate"
           \/ /\ pc' = "HostFlap"
        /\ UNCHANGED << jobs, nextID, lastRequest, lastJob, hostUp, stack, 
                        jobId, target_, host, target >>

FacadeRun == /\ pc = "FacadeRun"
             /\ lastRequest' = "run"
             /\ /\ host' = "hostA"
                /\ stack' = << [ procedure |->  "CreateJob",
                                 pc        |->  "Loop",
                                 host      |->  host,
                                 target    |->  target ] >>
                             \o stack
                /\ target' = Starting
             /\ pc' = "CreateJobDo"
             /\ UNCHANGED << jobs, nextID, lastJob, hostUp, jobId, target_ >>

FacadeQueue == /\ pc = "FacadeQueue"
               /\ lastRequest' = "queue"
               /\ /\ host' = "hostA"
                  /\ stack' = << [ procedure |->  "CreateJob",
                                   pc        |->  "Loop",
                                   host      |->  host,
                                   target    |->  target ] >>
                               \o stack
                  /\ target' = Queued
               /\ pc' = "CreateJobDo"
               /\ UNCHANGED << jobs, nextID, lastJob, hostUp, jobId, target_ >>

FacadeKill == /\ pc = "FacadeKill"
              /\ lastRequest' = "kill"
              /\ \E j \in DOMAIN jobs:
                   /\ /\ jobId' = j
                      /\ stack' = << [ procedure |->  "RecordIntent",
                                       pc        |->  "Loop",
                                       jobId     |->  jobId,
                                       target_   |->  target_ ] >>
                                   \o stack
                      /\ target_' = Killed
                   /\ pc' = "RecordIntentDo"
              /\ UNCHANGED << jobs, nextID, lastJob, hostUp, host, target >>

FacadeDraft == /\ pc = "FacadeDraft"
               /\ lastRequest' = "draft"
               /\ \E j \in DOMAIN jobs:
                    /\ /\ jobId' = j
                       /\ stack' = << [ procedure |->  "RecordIntent",
                                        pc        |->  "Loop",
                                        jobId     |->  jobId,
                                        target_   |->  target_ ] >>
                                    \o stack
                       /\ target_' = Draft
                    /\ pc' = "RecordIntentDo"
               /\ UNCHANGED << jobs, nextID, lastJob, hostUp, host, target >>

FacadeRestart == /\ pc = "FacadeRestart"
                 /\ lastRequest' = "restart"
                 /\ \E j \in DOMAIN jobs:
                      /\ /\ jobId' = j
                         /\ stack' = << [ procedure |->  "RecordIntent",
                                          pc        |->  "Loop",
                                          jobId     |->  jobId,
                                          target_   |->  target_ ] >>
                                      \o stack
                         /\ target_' = Queued
                      /\ pc' = "RecordIntentDo"
                 /\ UNCHANGED << jobs, nextID, lastJob, hostUp, host, target >>

FacadeCancel == /\ pc = "FacadeCancel"
                /\ lastRequest' = "cancel"
                /\ \E j \in DOMAIN jobs:
                     /\ /\ jobId' = j
                        /\ stack' = << [ procedure |->  "RecordIntent",
                                         pc        |->  "Loop",
                                         jobId     |->  jobId,
                                         target_   |->  target_ ] >>
                                     \o stack
                        /\ target_' = Canceled
                     /\ pc' = "RecordIntentDo"
                /\ UNCHANGED << jobs, nextID, lastJob, hostUp, host, target >>

FacadeUpdate == /\ pc = "FacadeUpdate"
                /\ lastRequest' = "update"
                /\ \E j \in DOMAIN jobs:
                     jobs' = [jobs EXCEPT ![j] = [status |-> jobs[j].status, pending |-> jobs[j].pending, host |-> jobs[j].host]]
                /\ pc' = "Loop"
                /\ UNCHANGED << nextID, lastJob, hostUp, stack, jobId, target_, 
                                host, target >>

HostFlap == /\ pc = "HostFlap"
            /\ \E h \in HostSet:
                 hostUp' = [hostUp EXCEPT ![h] = ~hostUp[h]]
            /\ pc' = "Loop"
            /\ UNCHANGED << jobs, nextID, lastRequest, lastJob, stack, jobId, 
                            target_, host, target >>

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == pc = "Done" /\ UNCHANGED vars

Next == RecordIntent \/ CreateJob \/ InitState \/ Loop \/ FacadeRun
           \/ FacadeQueue \/ FacadeKill \/ FacadeDraft \/ FacadeRestart
           \/ FacadeCancel \/ FacadeUpdate \/ HostFlap
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
    /\ lastRequest \in RequestSet
    /\ lastJob \in (JobID \cup {0})
    /\ hostUp \in [HostSet -> BOOLEAN]

=============================================================================
