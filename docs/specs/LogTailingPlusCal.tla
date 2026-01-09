------------------------------- MODULE LogTailingPlusCal -------------------------------
EXTENDS TLC, Sequences

Draft == "draft"
Queued == "queued"
Running == "running"
Completed == "completed"
Dead == "dead"
Failed == "failed"

LogMissing == "missing"
LogOpen == "open"
LogClosed == "closed"

JobIds == {1, 2}
StatusSet == {Draft, Queued, Running, Completed, Dead, Failed}
TerminalWait == {Completed, Dead, Failed}
LogStateSet == {LogMissing, LogOpen, LogClosed}

NextStatus(s) ==
    CASE s = Draft -> {Draft, Queued}
        [] s = Queued -> {Queued, Running}
        [] s = Running -> {Running, Completed, Dead, Failed}
        [] s = Completed -> {Completed}
        [] s = Dead -> {Dead}
        [] s = Failed -> {Failed}
        [] OTHER -> {s}

(* --algorithm LogTailing
variables
    remoteStatus \in [JobIds -> StatusSet],
    remoteLogState \in [JobIds -> LogStateSet],
    remoteLogSize \in [JobIds -> Nat],
    cacheLogState \in [JobIds -> LogStateSet],
    cacheLogSize \in [JobIds -> Nat],
    tailOffset \in [JobIds -> Nat],
    hostUp \in [JobIds -> BOOLEAN],
    clock \in Nat,
    timeout \in Nat,
    tailStart \in Nat,
    tailing \in BOOLEAN,
    doneReason \in {"none", "complete", "timeout"};

define
    TimedOut == clock - tailStart >= timeout;
    LogsFullyRead == \A j \in JobIds:
        (cacheLogState[j] = LogClosed) => tailOffset[j] = cacheLogSize[j];
end define;

procedure TailStep()
begin
    TailLoop:
        with j \in JobIds do
            if cacheLogState[j] = LogMissing then
                skip;
            elsif cacheLogSize[j] > tailOffset[j] then
                tailOffset[j] := cacheLogSize[j];
            end if;
        end with;
    TailReturn:
        return;
end procedure;

begin
InitState:
    remoteStatus := [j \in JobIds |-> Draft];
    remoteLogState := [j \in JobIds |-> LogMissing];
    remoteLogSize := [j \in JobIds |-> 0];
    cacheLogState := [j \in JobIds |-> LogMissing];
    cacheLogSize := [j \in JobIds |-> 0];
    tailOffset := [j \in JobIds |-> 0];
    hostUp := [j \in JobIds |-> TRUE];
    clock := 0;
    timeout := 6;
    tailStart := 0;
    tailing := TRUE;
    doneReason := "none";

Loop:
    while TRUE do
        either
            TimeTick:
                clock := clock + 1;
        or
            HostFlap:
                with j \in JobIds do
                    hostUp[j] := ~hostUp[j];
                end with;
        or
            RemoteProgress:
                with j \in JobIds do
                    if hostUp[j] then
                        with next \in NextStatus(remoteStatus[j]) do
                            remoteStatus[j] := next;
                        end with;
                        if remoteStatus[j] = Running /\ remoteLogState[j] = LogMissing then
                            remoteLogState[j] := LogOpen;
                        elsif remoteStatus[j] \in TerminalWait /\ remoteLogState[j] = LogOpen then
                            remoteLogState[j] := LogClosed;
                        end if;
                        if remoteLogState[j] = LogOpen then
                            remoteLogSize[j] := remoteLogSize[j] + 1;
                        end if;
                    end if;
                end with;
        or
            SyncTick:
                with j \in JobIds do
                    if hostUp[j] then
                        cacheLogState[j] := remoteLogState[j];
                        cacheLogSize[j] := remoteLogSize[j];
                    end if;
                end with;
        or
            TailTick:
                if tailing then
                    call TailStep();
                    if LogsFullyRead /\ (\A j \in JobIds: remoteStatus[j] \in TerminalWait) then
                        tailing := FALSE;
                        doneReason := "complete";
                    elsif TimedOut then
                        tailing := FALSE;
                        doneReason := "timeout";
                    end if;
                end if;
        end either;
    end while;
end algorithm; *)

TypeInvariant ==
    /\ remoteStatus \in [JobIds -> StatusSet]
    /\ remoteLogState \in [JobIds -> LogStateSet]
    /\ remoteLogSize \in [JobIds -> Nat]
    /\ cacheLogState \in [JobIds -> LogStateSet]
    /\ cacheLogSize \in [JobIds -> Nat]
    /\ tailOffset \in [JobIds -> Nat]
    /\ hostUp \in [JobIds -> BOOLEAN]
    /\ clock \in Nat
    /\ timeout \in Nat
    /\ tailStart \in Nat
    /\ tailing \in BOOLEAN
    /\ doneReason \in {"none", "complete", "timeout"}

TailCompletion ==
    (doneReason = "complete") =>
        (\A j \in JobIds: remoteStatus[j] \in TerminalWait)

TimeoutCompletion ==
    (doneReason = "timeout") => clock - tailStart >= timeout

=============================================================================
