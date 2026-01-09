------------------------------- MODULE WaitAndWatchPlusCal -------------------------------
EXTENDS TLC, Sequences

Draft == "draft"
Queued == "queued"
Running == "running"
Completed == "completed"
Dead == "dead"
Failed == "failed"

JobIds == {1, 2, 3}
StatusSet == {Draft, Queued, Running, Completed, Dead, Failed}
TerminalWait == {Completed, Dead, Failed}
ReportModeSet == {"all", "terminal"}
ReportModeConst \in ReportModeSet

NextStatus(s) ==
    CASE s = Draft -> {Draft, Queued}
        [] s = Queued -> {Queued, Running}
        [] s = Running -> {Running, Completed, Dead, Failed}
        [] s = Completed -> {Completed}
        [] s = Dead -> {Dead}
        [] s = Failed -> {Failed}
        [] OTHER -> {s}

(* --algorithm WaitAndWatch
variables
    remoteStatus \in [JobIds -> StatusSet],
    dbStatus \in [JobIds -> StatusSet],
    lastReported \in [JobIds -> StatusSet],
    reportedTerminals \in SUBSET JobIds,
    hostUp \in [JobIds -> BOOLEAN],
    clock \in Nat,
    timeout \in Nat,
    waitStart \in Nat,
    waiting \in BOOLEAN,
    doneReason \in {"none", "complete", "timeout"};

define
    AllTerminal == \A j \in JobIds: dbStatus[j] \in TerminalWait;
    TimedOut == clock - waitStart >= timeout;
end define;

procedure EmitUpdates()
begin
    EmitLoop:
        with j \in JobIds do
            if dbStatus[j] # lastReported[j] then
                if ReportModeConst = "all" \/ dbStatus[j] \in TerminalWait then
                    lastReported[j] := dbStatus[j];
                end if;
            end if;
            if dbStatus[j] \in TerminalWait /\ ~(j \in reportedTerminals) then
                reportedTerminals := reportedTerminals \cup {j};
            end if;
        end with;
    EmitReturn:
        return;
end procedure;

begin
InitState:
    remoteStatus := [j \in JobIds |-> Draft];
    dbStatus := [j \in JobIds |-> Draft];
    lastReported := [j \in JobIds |-> Draft];
    reportedTerminals := {};
    hostUp := [j \in JobIds |-> TRUE];
    clock := 0;
    timeout := 5;
    waitStart := 0;
    waiting := TRUE;
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
                    end if;
                end with;
        or
            SyncTick:
                with j \in JobIds do
                    if hostUp[j] then
                        dbStatus[j] := remoteStatus[j];
                    end if;
                end with;
        or
            WaitTick:
                if waiting then
                    call EmitUpdates();
                    if AllTerminal then
                        waiting := FALSE;
                        doneReason := "complete";
                    elsif TimedOut then
                        waiting := FALSE;
                        doneReason := "timeout";
                    end if;
                end if;
        end either;
    end while;
end algorithm; *)

TypeInvariant ==
    /\ remoteStatus \in [JobIds -> StatusSet]
    /\ dbStatus \in [JobIds -> StatusSet]
    /\ lastReported \in [JobIds -> StatusSet]
    /\ reportedTerminals \subseteq JobIds
    /\ hostUp \in [JobIds -> BOOLEAN]
    /\ clock \in Nat
    /\ timeout \in Nat
    /\ waitStart \in Nat
    /\ waiting \in BOOLEAN
    /\ doneReason \in {"none", "complete", "timeout"}
    /\ ReportModeConst \in ReportModeSet

WaitCompletion ==
    (doneReason = "complete") => \A j \in JobIds: dbStatus[j] \in TerminalWait

TimeoutCompletion ==
    (doneReason = "timeout") => clock - waitStart >= timeout

=============================================================================
