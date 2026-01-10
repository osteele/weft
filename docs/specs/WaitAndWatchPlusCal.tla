------------------------------- MODULE WaitAndWatchPlusCal -------------------------------
EXTENDS TLC, Sequences, Naturals

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
CONSTANT ReportModeConst
ASSUME ReportModeConst \in ReportModeSet

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
    remoteStatus = [j \in JobIds |-> Draft],
    dbStatus = [j \in JobIds |-> Draft],
    lastReported = [j \in JobIds |-> Draft],
    reportedTerminals = {},
    hostUp = [j \in JobIds |-> TRUE],
    clock = 0,
    timeout = 3,
    waitStart = 0,
    waiting = FALSE,
    doneReason = "none";

define
    AllTerminal == \A j \in JobIds: dbStatus[j] \in TerminalWait
    TimedOut == clock - waitStart >= timeout
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
                    CheckDone:
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
\* BEGIN TRANSLATION (chksum(pcal) = "490ac520" /\ chksum(tla) = "1ffcd")
VARIABLES remoteStatus, dbStatus, lastReported, reportedTerminals, hostUp, 
          clock, timeout, waitStart, waiting, doneReason, pc, stack

(* define statement *)
AllTerminal == \A j \in JobIds: dbStatus[j] \in TerminalWait
TimedOut == clock - waitStart >= timeout


vars == << remoteStatus, dbStatus, lastReported, reportedTerminals, hostUp, 
           clock, timeout, waitStart, waiting, doneReason, pc, stack >>

Init == (* Global variables *)
        /\ remoteStatus = [j \in JobIds |-> Draft]
        /\ dbStatus = [j \in JobIds |-> Draft]
        /\ lastReported = [j \in JobIds |-> Draft]
        /\ reportedTerminals = {}
        /\ hostUp = [j \in JobIds |-> TRUE]
        /\ clock = 0
        /\ timeout = 3
        /\ waitStart = 0
        /\ waiting = FALSE
        /\ doneReason = "none"
        /\ stack = << >>
        /\ pc = "InitState"

EmitLoop == /\ pc = "EmitLoop"
            /\ \E j \in JobIds:
                 /\ IF dbStatus[j] # lastReported[j]
                       THEN /\ IF ReportModeConst = "all" \/ dbStatus[j] \in TerminalWait
                                  THEN /\ lastReported' = [lastReported EXCEPT ![j] = dbStatus[j]]
                                  ELSE /\ TRUE
                                       /\ UNCHANGED lastReported
                       ELSE /\ TRUE
                            /\ UNCHANGED lastReported
                 /\ IF dbStatus[j] \in TerminalWait /\ ~(j \in reportedTerminals)
                       THEN /\ reportedTerminals' = (reportedTerminals \cup {j})
                       ELSE /\ TRUE
                            /\ UNCHANGED reportedTerminals
            /\ pc' = "EmitReturn"
            /\ UNCHANGED << remoteStatus, dbStatus, hostUp, clock, timeout, 
                            waitStart, waiting, doneReason, stack >>

EmitReturn == /\ pc = "EmitReturn"
              /\ pc' = Head(stack).pc
              /\ stack' = Tail(stack)
              /\ UNCHANGED << remoteStatus, dbStatus, lastReported, 
                              reportedTerminals, hostUp, clock, timeout, 
                              waitStart, waiting, doneReason >>

EmitUpdates == EmitLoop \/ EmitReturn

InitState == /\ pc = "InitState"
             /\ remoteStatus' = [j \in JobIds |-> Draft]
             /\ dbStatus' = [j \in JobIds |-> Draft]
             /\ lastReported' = [j \in JobIds |-> Draft]
             /\ reportedTerminals' = {}
             /\ hostUp' = [j \in JobIds |-> TRUE]
             /\ clock' = 0
             /\ timeout' = 5
             /\ waitStart' = 0
             /\ waiting' = TRUE
             /\ doneReason' = "none"
             /\ pc' = "Loop"
             /\ stack' = stack

Loop == /\ pc = "Loop"
        /\ \/ /\ pc' = "TimeTick"
           \/ /\ pc' = "HostFlap"
           \/ /\ pc' = "RemoteProgress"
           \/ /\ pc' = "SyncTick"
           \/ /\ pc' = "WaitTick"
        /\ UNCHANGED << remoteStatus, dbStatus, lastReported, 
                        reportedTerminals, hostUp, clock, timeout, waitStart, 
                        waiting, doneReason, stack >>

TimeTick == /\ pc = "TimeTick"
            /\ clock' = clock + 1
            /\ pc' = "Loop"
            /\ UNCHANGED << remoteStatus, dbStatus, lastReported, 
                            reportedTerminals, hostUp, timeout, waitStart, 
                            waiting, doneReason, stack >>

HostFlap == /\ pc = "HostFlap"
            /\ \E j \in JobIds:
                 hostUp' = [hostUp EXCEPT ![j] = ~hostUp[j]]
            /\ pc' = "Loop"
            /\ UNCHANGED << remoteStatus, dbStatus, lastReported, 
                            reportedTerminals, clock, timeout, waitStart, 
                            waiting, doneReason, stack >>

RemoteProgress == /\ pc = "RemoteProgress"
                  /\ \E j \in JobIds:
                       IF hostUp[j]
                          THEN /\ \E next \in NextStatus(remoteStatus[j]):
                                    remoteStatus' = [remoteStatus EXCEPT ![j] = next]
                          ELSE /\ TRUE
                               /\ UNCHANGED remoteStatus
                  /\ pc' = "Loop"
                  /\ UNCHANGED << dbStatus, lastReported, reportedTerminals, 
                                  hostUp, clock, timeout, waitStart, waiting, 
                                  doneReason, stack >>

SyncTick == /\ pc = "SyncTick"
            /\ \E j \in JobIds:
                 IF hostUp[j]
                    THEN /\ dbStatus' = [dbStatus EXCEPT ![j] = remoteStatus[j]]
                    ELSE /\ TRUE
                         /\ UNCHANGED dbStatus
            /\ pc' = "Loop"
            /\ UNCHANGED << remoteStatus, lastReported, reportedTerminals, 
                            hostUp, clock, timeout, waitStart, waiting, 
                            doneReason, stack >>

WaitTick == /\ pc = "WaitTick"
            /\ IF waiting
                  THEN /\ stack' = << [ procedure |->  "EmitUpdates",
                                        pc        |->  "CheckDone" ] >>
                                    \o stack
                       /\ pc' = "EmitLoop"
                  ELSE /\ pc' = "Loop"
                       /\ stack' = stack
            /\ UNCHANGED << remoteStatus, dbStatus, lastReported, 
                            reportedTerminals, hostUp, clock, timeout, 
                            waitStart, waiting, doneReason >>

CheckDone == /\ pc = "CheckDone"
             /\ IF AllTerminal
                   THEN /\ waiting' = FALSE
                        /\ doneReason' = "complete"
                   ELSE /\ IF TimedOut
                              THEN /\ waiting' = FALSE
                                   /\ doneReason' = "timeout"
                              ELSE /\ TRUE
                                   /\ UNCHANGED << waiting, doneReason >>
             /\ pc' = "Loop"
             /\ UNCHANGED << remoteStatus, dbStatus, lastReported, 
                             reportedTerminals, hostUp, clock, timeout, 
                             waitStart, stack >>

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == pc = "Done" /\ UNCHANGED vars

Next == EmitUpdates \/ InitState \/ Loop \/ TimeTick \/ HostFlap
           \/ RemoteProgress \/ SyncTick \/ WaitTick \/ CheckDone
           \/ Terminating

Spec == Init /\ [][Next]_vars

Termination == <>(pc = "Done")

\* END TRANSLATION 

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

\* State constraint to bound clock for model checking
StateConstraint == clock < 10

=============================================================================
