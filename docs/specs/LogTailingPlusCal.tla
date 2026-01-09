------------------------------- MODULE LogTailingPlusCal -------------------------------
EXTENDS TLC, Sequences, Naturals

Draft == "draft"
Queued == "queued"
Running == "running"
Completed == "completed"
Dead == "dead"
Failed == "failed"

LogMissing == "missing"
LogOpen == "open"
LogClosed == "closed"

\* Bound for model checking
MaxClock == 6

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
    remoteStatus = [j \in JobIds |-> Draft],
    remoteLogState = [j \in JobIds |-> LogMissing],
    remoteLogSize = [j \in JobIds |-> 0],
    cacheLogState = [j \in JobIds |-> LogMissing],
    cacheLogSize = [j \in JobIds |-> 0],
    tailOffset = [j \in JobIds |-> 0],
    hostUp = [j \in JobIds |-> TRUE],
    clock = 0,
    timeout = 6,
    tailStart = 0,
    tailing = TRUE,
    doneReason = "none";

define
    TimedOut == clock - tailStart >= timeout
    LogsFullyRead == \A j \in JobIds:
        (cacheLogState[j] = LogClosed) => tailOffset[j] = cacheLogSize[j]
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
                TailTickPost:
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
\* BEGIN TRANSLATION (chksum(pcal) = "bd378ac7" /\ chksum(tla) = "e412e971")
VARIABLES remoteStatus, remoteLogState, remoteLogSize, cacheLogState, 
          cacheLogSize, tailOffset, hostUp, clock, timeout, tailStart, 
          tailing, doneReason, pc, stack

(* define statement *)
TimedOut == clock - tailStart >= timeout
LogsFullyRead == \A j \in JobIds:
    (cacheLogState[j] = LogClosed) => tailOffset[j] = cacheLogSize[j]


vars == << remoteStatus, remoteLogState, remoteLogSize, cacheLogState, 
           cacheLogSize, tailOffset, hostUp, clock, timeout, tailStart, 
           tailing, doneReason, pc, stack >>

Init == (* Global variables *)
        /\ remoteStatus = [j \in JobIds |-> Draft]
        /\ remoteLogState = [j \in JobIds |-> LogMissing]
        /\ remoteLogSize = [j \in JobIds |-> 0]
        /\ cacheLogState = [j \in JobIds |-> LogMissing]
        /\ cacheLogSize = [j \in JobIds |-> 0]
        /\ tailOffset = [j \in JobIds |-> 0]
        /\ hostUp = [j \in JobIds |-> TRUE]
        /\ clock = 0
        /\ timeout = 6
        /\ tailStart = 0
        /\ tailing = TRUE
        /\ doneReason = "none"
        /\ stack = << >>
        /\ pc = "InitState"

TailLoop == /\ pc = "TailLoop"
            /\ \E j \in JobIds:
                 IF cacheLogState[j] = LogMissing
                    THEN /\ TRUE
                         /\ UNCHANGED tailOffset
                    ELSE /\ IF cacheLogSize[j] > tailOffset[j]
                               THEN /\ tailOffset' = [tailOffset EXCEPT ![j] = cacheLogSize[j]]
                               ELSE /\ TRUE
                                    /\ UNCHANGED tailOffset
            /\ pc' = "TailReturn"
            /\ UNCHANGED << remoteStatus, remoteLogState, remoteLogSize, 
                            cacheLogState, cacheLogSize, hostUp, clock, 
                            timeout, tailStart, tailing, doneReason, stack >>

TailReturn == /\ pc = "TailReturn"
              /\ pc' = Head(stack).pc
              /\ stack' = Tail(stack)
              /\ UNCHANGED << remoteStatus, remoteLogState, remoteLogSize, 
                              cacheLogState, cacheLogSize, tailOffset, hostUp, 
                              clock, timeout, tailStart, tailing, doneReason >>

TailStep == TailLoop \/ TailReturn

InitState == /\ pc = "InitState"
             /\ remoteStatus' = [j \in JobIds |-> Draft]
             /\ remoteLogState' = [j \in JobIds |-> LogMissing]
             /\ remoteLogSize' = [j \in JobIds |-> 0]
             /\ cacheLogState' = [j \in JobIds |-> LogMissing]
             /\ cacheLogSize' = [j \in JobIds |-> 0]
             /\ tailOffset' = [j \in JobIds |-> 0]
             /\ hostUp' = [j \in JobIds |-> TRUE]
             /\ clock' = 0
             /\ timeout' = 6
             /\ tailStart' = 0
             /\ tailing' = TRUE
             /\ doneReason' = "none"
             /\ pc' = "Loop"
             /\ stack' = stack

Loop == /\ pc = "Loop"
        /\ \/ /\ pc' = "TimeTick"
           \/ /\ pc' = "HostFlap"
           \/ /\ pc' = "RemoteProgress"
           \/ /\ pc' = "SyncTick"
           \/ /\ pc' = "TailTick"
        /\ UNCHANGED << remoteStatus, remoteLogState, remoteLogSize, 
                        cacheLogState, cacheLogSize, tailOffset, hostUp, clock, 
                        timeout, tailStart, tailing, doneReason, stack >>

TimeTick == /\ pc = "TimeTick"
            /\ clock' = clock + 1
            /\ pc' = "Loop"
            /\ UNCHANGED << remoteStatus, remoteLogState, remoteLogSize, 
                            cacheLogState, cacheLogSize, tailOffset, hostUp, 
                            timeout, tailStart, tailing, doneReason, stack >>

HostFlap == /\ pc = "HostFlap"
            /\ \E j \in JobIds:
                 hostUp' = [hostUp EXCEPT ![j] = ~hostUp[j]]
            /\ pc' = "Loop"
            /\ UNCHANGED << remoteStatus, remoteLogState, remoteLogSize, 
                            cacheLogState, cacheLogSize, tailOffset, clock, 
                            timeout, tailStart, tailing, doneReason, stack >>

RemoteProgress == /\ pc = "RemoteProgress"
                  /\ \E j \in JobIds:
                       IF hostUp[j]
                          THEN /\ \E next \in NextStatus(remoteStatus[j]):
                                    remoteStatus' = [remoteStatus EXCEPT ![j] = next]
                               /\ IF remoteStatus'[j] = Running /\ remoteLogState[j] = LogMissing
                                     THEN /\ remoteLogState' = [remoteLogState EXCEPT ![j] = LogOpen]
                                     ELSE /\ IF remoteStatus'[j] \in TerminalWait /\ remoteLogState[j] = LogOpen
                                                THEN /\ remoteLogState' = [remoteLogState EXCEPT ![j] = LogClosed]
                                                ELSE /\ TRUE
                                                     /\ UNCHANGED remoteLogState
                               /\ IF remoteLogState'[j] = LogOpen
                                     THEN /\ remoteLogSize' = [remoteLogSize EXCEPT ![j] = remoteLogSize[j] + 1]
                                     ELSE /\ TRUE
                                          /\ UNCHANGED remoteLogSize
                          ELSE /\ TRUE
                               /\ UNCHANGED << remoteStatus, remoteLogState, 
                                               remoteLogSize >>
                  /\ pc' = "Loop"
                  /\ UNCHANGED << cacheLogState, cacheLogSize, tailOffset, 
                                  hostUp, clock, timeout, tailStart, tailing, 
                                  doneReason, stack >>

SyncTick == /\ pc = "SyncTick"
            /\ \E j \in JobIds:
                 IF hostUp[j]
                    THEN /\ cacheLogState' = [cacheLogState EXCEPT ![j] = remoteLogState[j]]
                         /\ cacheLogSize' = [cacheLogSize EXCEPT ![j] = remoteLogSize[j]]
                    ELSE /\ TRUE
                         /\ UNCHANGED << cacheLogState, cacheLogSize >>
            /\ pc' = "Loop"
            /\ UNCHANGED << remoteStatus, remoteLogState, remoteLogSize, 
                            tailOffset, hostUp, clock, timeout, tailStart, 
                            tailing, doneReason, stack >>

TailTick == /\ pc = "TailTick"
            /\ IF tailing
                  THEN /\ stack' = << [ procedure |->  "TailStep",
                                        pc        |->  "TailTickPost" ] >>
                                    \o stack
                       /\ pc' = "TailLoop"
                  ELSE /\ pc' = "Loop"
                       /\ stack' = stack
            /\ UNCHANGED << remoteStatus, remoteLogState, remoteLogSize, 
                            cacheLogState, cacheLogSize, tailOffset, hostUp, 
                            clock, timeout, tailStart, tailing, doneReason >>

TailTickPost == /\ pc = "TailTickPost"
                /\ IF LogsFullyRead /\ (\A j \in JobIds: remoteStatus[j] \in TerminalWait)
                      THEN /\ tailing' = FALSE
                           /\ doneReason' = "complete"
                      ELSE /\ IF TimedOut
                                 THEN /\ tailing' = FALSE
                                      /\ doneReason' = "timeout"
                                 ELSE /\ TRUE
                                      /\ UNCHANGED << tailing, doneReason >>
                /\ pc' = "Loop"
                /\ UNCHANGED << remoteStatus, remoteLogState, remoteLogSize, 
                                cacheLogState, cacheLogSize, tailOffset, 
                                hostUp, clock, timeout, tailStart, stack >>

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == pc = "Done" /\ UNCHANGED vars

Next == TailStep \/ InitState \/ Loop \/ TimeTick \/ HostFlap
           \/ RemoteProgress \/ SyncTick \/ TailTick \/ TailTickPost
           \/ Terminating

Spec == Init /\ [][Next]_vars

Termination == <>(pc = "Done")

\* END TRANSLATION 

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

\* State constraint for bounded model checking
StateConstraint ==
    /\ clock <= MaxClock
    /\ \A j \in JobIds: remoteLogSize[j] <= MaxClock
    /\ \A j \in JobIds: cacheLogSize[j] <= MaxClock

=============================================================================
