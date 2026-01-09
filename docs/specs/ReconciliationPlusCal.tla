------------------------------- MODULE ReconciliationPlusCal -------------------------------
EXTENDS TLC, Sequences

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
    status \in StatusSet,
    base \in StatusSet,
    pending \in PendingSet,
    remote \in RemoteStatusSet,
    queueHasEntry \in BOOLEAN,
    runnerActive \in BOOLEAN,
    runnerVersion \in Nat,
    runnerDesiredVersion \in Nat,
    hostUp \in BOOLEAN,
    connectionError \in BOOLEAN;

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
\* END TRANSLATION REMOVED (use PlusCal section above)
=============================================================================
