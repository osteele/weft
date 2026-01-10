------------------------------- MODULE ReconciliationPlusCal -------------------------------
EXTENDS TLC, Sequences, Naturals

\* High-level status vocabulary reverse-engineered from docs/architecture.md and
\* internal/ops/reconcile.go. The model purposely simplifies timestamps, SQL
\* bookkeeping, and queue metadata so that we can focus on the sync/reconcile
\* control flow.
Draft == "draft"
Queued == "queued"
Running == "running"
Completed == "completed"
Dead == "dead"
Failed == "failed"
Killed == "killed"
Canceled == "canceled"

NoPending == "no-pending"
NotPresent == "missing"

StatusSet == {Draft, Queued, Running, Completed, Dead, Failed, Killed, Canceled}
PendingSet == StatusSet \cup {NoPending}
RemoteStatusSet == StatusSet \cup {NotPresent}
TerminalSet == {Completed, Dead, Failed, Draft, Killed, Canceled}
DesiredIntents == {Draft, Queued, Running, Dead, Killed, Canceled}

RemoteChoices(r) ==
    CASE r = Draft -> {Draft, Queued}
        [] r = Queued -> {Queued, Running, NotPresent}
        [] r = Running -> {Running, Completed, Dead}
        [] r = Completed -> {Completed}
        [] r = Dead -> {Dead}
        [] r = Failed -> {Failed, Draft}
        [] r = NotPresent -> {NotPresent, Queued}
        [] OTHER -> {r}

(* --algorithm SyncAndReconcile
variables
    status = Draft,
    base = Draft,
    pending = NoPending,
    remote = NotPresent,
    queueHasEntry = FALSE,
    runnerActive = FALSE,
    runnerVersion = 0,
    runnerDesiredVersion = 0,
    hostUp = TRUE,
    connectionError = FALSE;

define
    PendingActive == pending # NoPending
    TerminalRemote == remote \in TerminalSet
    QueueMissing == (status = Queued) /\ (remote = NotPresent)
    RunnerNeedsStart == queueHasEntry /\ ~runnerActive
    RunnerNeedsStop == ~queueHasEntry /\ runnerActive
    RunnerNeedsUpdate == runnerActive /\ runnerDesiredVersion > runnerVersion
end define;

procedure AcceptRemote()
begin
    AcceptRemoteUpdate:
        status := remote;
        base := remote;
        pending := NoPending;
        queueHasEntry := remote \in {Queued, Running};
        connectionError := FALSE;
    AcceptRemoteReturn:
        return;
end procedure;

procedure ApplyPending(target)
begin
    ApplyPendingDecision:
        if ~hostUp then
            connectionError := TRUE;
        else
            remote := target;
            status := target;
            base := target;
            pending := NoPending;
            queueHasEntry := target \in {Queued, Running};
        end if;
    ApplyPendingReturn:
        return;
end procedure;

procedure EnsureQueueRunner()
begin
    RunnerStart:
        if RunnerNeedsStart then
            if hostUp then
                runnerActive := TRUE;
                runnerVersion := runnerDesiredVersion;
            else
                connectionError := TRUE;
            end if;
        end if;
    RunnerStop:
        if RunnerNeedsStop then
            if hostUp then
                runnerActive := FALSE;
            else
                connectionError := TRUE;
            end if;
        end if;
    RunnerUpdate:
        if RunnerNeedsUpdate then
            if hostUp then
                runnerVersion := runnerDesiredVersion;
            else
                connectionError := TRUE;
            end if;
        end if;
    RunnerReturn:
        return;
end procedure;

procedure ResolveConflict()
begin
    ResolvePolicy:
        if TerminalRemote then
            call AcceptRemote();
        else
            call ApplyPending(pending);
        end if;
    ResolveReturn:
        return;
end procedure;

procedure Reconcile()
begin
    ReconcileReset:
        connectionError := FALSE;
    ReconcileCases:
        if ~PendingActive /\ base = remote then
            skip;
        elsif ~PendingActive /\ base # remote then
            call AcceptRemote();
        elsif PendingActive /\ base = remote then
            call ApplyPending(pending);
        elsif PendingActive /\ pending = remote then
            call AcceptRemote();
        elsif QueueMissing then
            QueueRepair:
                if hostUp then
                    remote := Queued;
                    queueHasEntry := TRUE;
                    base := Queued;
                else
                    connectionError := TRUE;
                end if;
        else
            call ResolveConflict();
        end if;
    ReconcileRunner:
        call EnsureQueueRunner();
    ReconcileReturn:
        return;
end procedure;

begin
InitState:
    status := Draft;
    base := Draft;
    pending := NoPending;
    remote := NotPresent;
    queueHasEntry := FALSE;
    runnerActive := FALSE;
    runnerVersion := 0;
    runnerDesiredVersion := 0;
    hostUp := TRUE;
    connectionError := FALSE;

Loop:
    while TRUE do
        either
            UserIntent:
                await pending = NoPending;
                with desired \in DesiredIntents do
                    if desired # status then
                        pending := desired;
                    end if;
                end with;
        or
            RemoteProgress:
                with newRemote \in RemoteChoices(remote) do
                    remote := newRemote;
                    if newRemote \in {Queued, Running} then
                        queueHasEntry := TRUE;
                    elsif newRemote = NotPresent then
                        queueHasEntry := FALSE;
                    end if;
                end with;
        or
            RunnerConfigChange:
                runnerDesiredVersion := runnerDesiredVersion + 1;
        or
            SyncTick:
                call Reconcile();
        or
            HostFlap:
                hostUp := ~hostUp;
        end either;
    end while;
end algorithm; *)
\* BEGIN TRANSLATION (chksum(pcal) = "1c760e78" /\ chksum(tla) = "80b1b8be")
CONSTANT defaultInitValue
VARIABLES status, base, pending, remote, queueHasEntry, runnerActive, 
          runnerVersion, runnerDesiredVersion, hostUp, connectionError, pc, 
          stack

(* define statement *)
PendingActive == pending # NoPending
TerminalRemote == remote \in TerminalSet
QueueMissing == (status = Queued) /\ (remote = NotPresent)
RunnerNeedsStart == queueHasEntry /\ ~runnerActive
RunnerNeedsStop == ~queueHasEntry /\ runnerActive
RunnerNeedsUpdate == runnerActive /\ runnerDesiredVersion > runnerVersion

VARIABLE target

vars == << status, base, pending, remote, queueHasEntry, runnerActive, 
           runnerVersion, runnerDesiredVersion, hostUp, connectionError, pc, 
           stack, target >>

Init == (* Global variables *)
        /\ status = Draft
        /\ base = Draft
        /\ pending = NoPending
        /\ remote = NotPresent
        /\ queueHasEntry = FALSE
        /\ runnerActive = FALSE
        /\ runnerVersion = 0
        /\ runnerDesiredVersion = 0
        /\ hostUp = TRUE
        /\ connectionError = FALSE
        (* Procedure ApplyPending *)
        /\ target = defaultInitValue
        /\ stack = << >>
        /\ pc = "InitState"

AcceptRemoteUpdate == /\ pc = "AcceptRemoteUpdate"
                      /\ status' = remote
                      /\ base' = remote
                      /\ pending' = NoPending
                      /\ queueHasEntry' = (remote \in {Queued, Running})
                      /\ connectionError' = FALSE
                      /\ pc' = "AcceptRemoteReturn"
                      /\ UNCHANGED << remote, runnerActive, runnerVersion, 
                                      runnerDesiredVersion, hostUp, stack, 
                                      target >>

AcceptRemoteReturn == /\ pc = "AcceptRemoteReturn"
                      /\ pc' = Head(stack).pc
                      /\ stack' = Tail(stack)
                      /\ UNCHANGED << status, base, pending, remote, 
                                      queueHasEntry, runnerActive, 
                                      runnerVersion, runnerDesiredVersion, 
                                      hostUp, connectionError, target >>

AcceptRemote == AcceptRemoteUpdate \/ AcceptRemoteReturn

ApplyPendingDecision == /\ pc = "ApplyPendingDecision"
                        /\ IF ~hostUp
                              THEN /\ connectionError' = TRUE
                                   /\ UNCHANGED << status, base, pending, 
                                                   remote, queueHasEntry >>
                              ELSE /\ remote' = target
                                   /\ status' = target
                                   /\ base' = target
                                   /\ pending' = NoPending
                                   /\ queueHasEntry' = (target \in {Queued, Running})
                                   /\ UNCHANGED connectionError
                        /\ pc' = "ApplyPendingReturn"
                        /\ UNCHANGED << runnerActive, runnerVersion, 
                                        runnerDesiredVersion, hostUp, stack, 
                                        target >>

ApplyPendingReturn == /\ pc = "ApplyPendingReturn"
                      /\ pc' = Head(stack).pc
                      /\ target' = Head(stack).target
                      /\ stack' = Tail(stack)
                      /\ UNCHANGED << status, base, pending, remote, 
                                      queueHasEntry, runnerActive, 
                                      runnerVersion, runnerDesiredVersion, 
                                      hostUp, connectionError >>

ApplyPending == ApplyPendingDecision \/ ApplyPendingReturn

RunnerStart == /\ pc = "RunnerStart"
               /\ IF RunnerNeedsStart
                     THEN /\ IF hostUp
                                THEN /\ runnerActive' = TRUE
                                     /\ runnerVersion' = runnerDesiredVersion
                                     /\ UNCHANGED connectionError
                                ELSE /\ connectionError' = TRUE
                                     /\ UNCHANGED << runnerActive, 
                                                     runnerVersion >>
                     ELSE /\ TRUE
                          /\ UNCHANGED << runnerActive, runnerVersion, 
                                          connectionError >>
               /\ pc' = "RunnerStop"
               /\ UNCHANGED << status, base, pending, remote, queueHasEntry, 
                               runnerDesiredVersion, hostUp, stack, target >>

RunnerStop == /\ pc = "RunnerStop"
              /\ IF RunnerNeedsStop
                    THEN /\ IF hostUp
                               THEN /\ runnerActive' = FALSE
                                    /\ UNCHANGED connectionError
                               ELSE /\ connectionError' = TRUE
                                    /\ UNCHANGED runnerActive
                    ELSE /\ TRUE
                         /\ UNCHANGED << runnerActive, connectionError >>
              /\ pc' = "RunnerUpdate"
              /\ UNCHANGED << status, base, pending, remote, queueHasEntry, 
                              runnerVersion, runnerDesiredVersion, hostUp, 
                              stack, target >>

RunnerUpdate == /\ pc = "RunnerUpdate"
                /\ IF RunnerNeedsUpdate
                      THEN /\ IF hostUp
                                 THEN /\ runnerVersion' = runnerDesiredVersion
                                      /\ UNCHANGED connectionError
                                 ELSE /\ connectionError' = TRUE
                                      /\ UNCHANGED runnerVersion
                      ELSE /\ TRUE
                           /\ UNCHANGED << runnerVersion, connectionError >>
                /\ pc' = "RunnerReturn"
                /\ UNCHANGED << status, base, pending, remote, queueHasEntry, 
                                runnerActive, runnerDesiredVersion, hostUp, 
                                stack, target >>

RunnerReturn == /\ pc = "RunnerReturn"
                /\ pc' = Head(stack).pc
                /\ stack' = Tail(stack)
                /\ UNCHANGED << status, base, pending, remote, queueHasEntry, 
                                runnerActive, runnerVersion, 
                                runnerDesiredVersion, hostUp, connectionError, 
                                target >>

EnsureQueueRunner == RunnerStart \/ RunnerStop \/ RunnerUpdate
                        \/ RunnerReturn

ResolvePolicy == /\ pc = "ResolvePolicy"
                 /\ IF TerminalRemote
                       THEN /\ stack' = << [ procedure |->  "AcceptRemote",
                                             pc        |->  "ResolveReturn" ] >>
                                         \o stack
                            /\ pc' = "AcceptRemoteUpdate"
                            /\ UNCHANGED target
                       ELSE /\ /\ stack' = << [ procedure |->  "ApplyPending",
                                                pc        |->  "ResolveReturn",
                                                target    |->  target ] >>
                                            \o stack
                               /\ target' = pending
                            /\ pc' = "ApplyPendingDecision"
                 /\ UNCHANGED << status, base, pending, remote, queueHasEntry, 
                                 runnerActive, runnerVersion, 
                                 runnerDesiredVersion, hostUp, connectionError >>

ResolveReturn == /\ pc = "ResolveReturn"
                 /\ pc' = Head(stack).pc
                 /\ stack' = Tail(stack)
                 /\ UNCHANGED << status, base, pending, remote, queueHasEntry, 
                                 runnerActive, runnerVersion, 
                                 runnerDesiredVersion, hostUp, connectionError, 
                                 target >>

ResolveConflict == ResolvePolicy \/ ResolveReturn

ReconcileReset == /\ pc = "ReconcileReset"
                  /\ connectionError' = FALSE
                  /\ pc' = "ReconcileCases"
                  /\ UNCHANGED << status, base, pending, remote, queueHasEntry, 
                                  runnerActive, runnerVersion, 
                                  runnerDesiredVersion, hostUp, stack, target >>

ReconcileCases == /\ pc = "ReconcileCases"
                  /\ IF ~PendingActive /\ base = remote
                        THEN /\ TRUE
                             /\ pc' = "ReconcileRunner"
                             /\ UNCHANGED << stack, target >>
                        ELSE /\ IF ~PendingActive /\ base # remote
                                   THEN /\ stack' = << [ procedure |->  "AcceptRemote",
                                                         pc        |->  "ReconcileRunner" ] >>
                                                     \o stack
                                        /\ pc' = "AcceptRemoteUpdate"
                                        /\ UNCHANGED target
                                   ELSE /\ IF PendingActive /\ base = remote
                                              THEN /\ /\ stack' = << [ procedure |->  "ApplyPending",
                                                                       pc        |->  "ReconcileRunner",
                                                                       target    |->  target ] >>
                                                                   \o stack
                                                      /\ target' = pending
                                                   /\ pc' = "ApplyPendingDecision"
                                              ELSE /\ IF PendingActive /\ pending = remote
                                                         THEN /\ stack' = << [ procedure |->  "AcceptRemote",
                                                                               pc        |->  "ReconcileRunner" ] >>
                                                                           \o stack
                                                              /\ pc' = "AcceptRemoteUpdate"
                                                         ELSE /\ IF QueueMissing
                                                                    THEN /\ pc' = "QueueRepair"
                                                                         /\ stack' = stack
                                                                    ELSE /\ stack' = << [ procedure |->  "ResolveConflict",
                                                                                          pc        |->  "ReconcileRunner" ] >>
                                                                                      \o stack
                                                                         /\ pc' = "ResolvePolicy"
                                                   /\ UNCHANGED target
                  /\ UNCHANGED << status, base, pending, remote, queueHasEntry, 
                                  runnerActive, runnerVersion, 
                                  runnerDesiredVersion, hostUp, 
                                  connectionError >>

QueueRepair == /\ pc = "QueueRepair"
               /\ IF hostUp
                     THEN /\ remote' = Queued
                          /\ queueHasEntry' = TRUE
                          /\ base' = Queued
                          /\ UNCHANGED connectionError
                     ELSE /\ connectionError' = TRUE
                          /\ UNCHANGED << base, remote, queueHasEntry >>
               /\ pc' = "ReconcileRunner"
               /\ UNCHANGED << status, pending, runnerActive, runnerVersion, 
                               runnerDesiredVersion, hostUp, stack, target >>

ReconcileRunner == /\ pc = "ReconcileRunner"
                   /\ stack' = << [ procedure |->  "EnsureQueueRunner",
                                    pc        |->  "ReconcileReturn" ] >>
                                \o stack
                   /\ pc' = "RunnerStart"
                   /\ UNCHANGED << status, base, pending, remote, 
                                   queueHasEntry, runnerActive, runnerVersion, 
                                   runnerDesiredVersion, hostUp, 
                                   connectionError, target >>

ReconcileReturn == /\ pc = "ReconcileReturn"
                   /\ pc' = Head(stack).pc
                   /\ stack' = Tail(stack)
                   /\ UNCHANGED << status, base, pending, remote, 
                                   queueHasEntry, runnerActive, runnerVersion, 
                                   runnerDesiredVersion, hostUp, 
                                   connectionError, target >>

Reconcile == ReconcileReset \/ ReconcileCases \/ QueueRepair
                \/ ReconcileRunner \/ ReconcileReturn

InitState == /\ pc = "InitState"
             /\ status' = Draft
             /\ base' = Draft
             /\ pending' = NoPending
             /\ remote' = NotPresent
             /\ queueHasEntry' = FALSE
             /\ runnerActive' = FALSE
             /\ runnerVersion' = 0
             /\ runnerDesiredVersion' = 0
             /\ hostUp' = TRUE
             /\ connectionError' = FALSE
             /\ pc' = "Loop"
             /\ UNCHANGED << stack, target >>

Loop == /\ pc = "Loop"
        /\ \/ /\ pc' = "UserIntent"
           \/ /\ pc' = "RemoteProgress"
           \/ /\ pc' = "RunnerConfigChange"
           \/ /\ pc' = "SyncTick"
           \/ /\ pc' = "HostFlap"
        /\ UNCHANGED << status, base, pending, remote, queueHasEntry, 
                        runnerActive, runnerVersion, runnerDesiredVersion, 
                        hostUp, connectionError, stack, target >>

UserIntent == /\ pc = "UserIntent"
              /\ pending = NoPending
              /\ \E desired \in DesiredIntents:
                   IF desired # status
                      THEN /\ pending' = desired
                      ELSE /\ TRUE
                           /\ UNCHANGED pending
              /\ pc' = "Loop"
              /\ UNCHANGED << status, base, remote, queueHasEntry, 
                              runnerActive, runnerVersion, 
                              runnerDesiredVersion, hostUp, connectionError, 
                              stack, target >>

RemoteProgress == /\ pc = "RemoteProgress"
                  /\ \E newRemote \in RemoteChoices(remote):
                       /\ remote' = newRemote
                       /\ IF newRemote \in {Queued, Running}
                             THEN /\ queueHasEntry' = TRUE
                             ELSE /\ IF newRemote = NotPresent
                                        THEN /\ queueHasEntry' = FALSE
                                        ELSE /\ TRUE
                                             /\ UNCHANGED queueHasEntry
                  /\ pc' = "Loop"
                  /\ UNCHANGED << status, base, pending, runnerActive, 
                                  runnerVersion, runnerDesiredVersion, hostUp, 
                                  connectionError, stack, target >>

RunnerConfigChange == /\ pc = "RunnerConfigChange"
                      /\ runnerDesiredVersion' = runnerDesiredVersion + 1
                      /\ pc' = "Loop"
                      /\ UNCHANGED << status, base, pending, remote, 
                                      queueHasEntry, runnerActive, 
                                      runnerVersion, hostUp, connectionError, 
                                      stack, target >>

SyncTick == /\ pc = "SyncTick"
            /\ stack' = << [ procedure |->  "Reconcile",
                             pc        |->  "Loop" ] >>
                         \o stack
            /\ pc' = "ReconcileReset"
            /\ UNCHANGED << status, base, pending, remote, queueHasEntry, 
                            runnerActive, runnerVersion, runnerDesiredVersion, 
                            hostUp, connectionError, target >>

HostFlap == /\ pc = "HostFlap"
            /\ hostUp' = ~hostUp
            /\ pc' = "Loop"
            /\ UNCHANGED << status, base, pending, remote, queueHasEntry, 
                            runnerActive, runnerVersion, runnerDesiredVersion, 
                            connectionError, stack, target >>

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == pc = "Done" /\ UNCHANGED vars

Next == AcceptRemote \/ ApplyPending \/ EnsureQueueRunner
           \/ ResolveConflict \/ Reconcile \/ InitState \/ Loop \/ UserIntent
           \/ RemoteProgress \/ RunnerConfigChange \/ SyncTick \/ HostFlap
           \/ Terminating

Spec == Init /\ [][Next]_vars

Termination == <>(pc = "Done")

\* END TRANSLATION
\* END TRANSLATION REMOVED (use PlusCal section above)

\* State constraint to bound state space for model checking
StateConstraint == runnerDesiredVersion < 3

=============================================================================
