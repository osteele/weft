----------------------------- MODULE LoggingProgressPlusCal -----------------------------
EXTENDS TLC, Sequences, Integers

\* Models log files, streaming, caching, and progress parsing.

ModeSet == {"idle", "streaming", "following"}
LogStateSet == {"missing", "remote", "cached"}
SourceSet == {"none", "remote", "cached"}

\* Bound for model checking
CONSTANT MaxProgress
ProgressRange == -1..MaxProgress

(* --algorithm LoggingProgress
variables
    mode = "idle",
    remoteLog = "missing",
    cachedLog = "missing",
    activeSource = "none",
    warnedFallback = FALSE,
    lastProgress = -1,
    jobDone = FALSE,
    networkUp = TRUE;

begin
InitState:
    mode := "idle";
    remoteLog := "missing";
    cachedLog := "missing";
    activeSource := "none";
    warnedFallback := FALSE;
    lastProgress := -1;
    jobDone := FALSE;
    networkUp := TRUE;

Loop:
    while TRUE do
        either
            RemoteLogAppears:
                remoteLog := "remote";
        or
            CacheLog:
                if remoteLog = "remote" then
                    cachedLog := "cached";
                end if;
        or
            StartStream:
                \* run --allow
                if remoteLog = "remote" then
                    mode := "streaming";
                end if;
        or
            StartFollow:
                \* log -f
                if jobDone /\ cachedLog = "cached" then
                    mode := "following";
                    activeSource := "cached";
                    warnedFallback := FALSE;
                elsif remoteLog = "remote" /\ networkUp then
                    mode := "following";
                    activeSource := "remote";
                    warnedFallback := FALSE;
                elsif cachedLog = "cached" then
                    mode := "following";
                    activeSource := "cached";
                    warnedFallback := TRUE;
                end if;
        or
            StopFollow:
                mode := "idle";
                activeSource := "none";
                warnedFallback := FALSE;
        or
            ParseProgress:
                \* progress comes from log tail; store latest integer percent
                with p \in 0..MaxProgress do
                    lastProgress := p;
                end with;
        or
            MarkJobDone:
                jobDone := TRUE;
        or
            NetworkFlap:
                networkUp := ~networkUp;
        end either;
    end while;
end algorithm; *)
\* BEGIN TRANSLATION (chksum(pcal) = "8d2cdf8a" /\ chksum(tla) = "f85b9f34")
VARIABLES mode, remoteLog, cachedLog, activeSource, warnedFallback, 
          lastProgress, jobDone, networkUp, pc

vars == << mode, remoteLog, cachedLog, activeSource, warnedFallback, 
           lastProgress, jobDone, networkUp, pc >>

Init == (* Global variables *)
        /\ mode = "idle"
        /\ remoteLog = "missing"
        /\ cachedLog = "missing"
        /\ activeSource = "none"
        /\ warnedFallback = FALSE
        /\ lastProgress = -1
        /\ jobDone = FALSE
        /\ networkUp = TRUE
        /\ pc = "InitState"

InitState == /\ pc = "InitState"
             /\ mode' = "idle"
             /\ remoteLog' = "missing"
             /\ cachedLog' = "missing"
             /\ activeSource' = "none"
             /\ warnedFallback' = FALSE
             /\ lastProgress' = -1
             /\ jobDone' = FALSE
             /\ networkUp' = TRUE
             /\ pc' = "Loop"

Loop == /\ pc = "Loop"
        /\ \/ /\ pc' = "RemoteLogAppears"
           \/ /\ pc' = "CacheLog"
           \/ /\ pc' = "StartStream"
           \/ /\ pc' = "StartFollow"
           \/ /\ pc' = "StopFollow"
           \/ /\ pc' = "ParseProgress"
           \/ /\ pc' = "MarkJobDone"
           \/ /\ pc' = "NetworkFlap"
        /\ UNCHANGED << mode, remoteLog, cachedLog, activeSource, 
                        warnedFallback, lastProgress, jobDone, networkUp >>

RemoteLogAppears == /\ pc = "RemoteLogAppears"
                    /\ remoteLog' = "remote"
                    /\ pc' = "Loop"
                    /\ UNCHANGED << mode, cachedLog, activeSource, 
                                    warnedFallback, lastProgress, jobDone, 
                                    networkUp >>

CacheLog == /\ pc = "CacheLog"
            /\ IF remoteLog = "remote"
                  THEN /\ cachedLog' = "cached"
                  ELSE /\ TRUE
                       /\ UNCHANGED cachedLog
            /\ pc' = "Loop"
            /\ UNCHANGED << mode, remoteLog, activeSource, warnedFallback, 
                            lastProgress, jobDone, networkUp >>

StartStream == /\ pc = "StartStream"
               /\ IF remoteLog = "remote"
                     THEN /\ mode' = "streaming"
                     ELSE /\ TRUE
                          /\ mode' = mode
               /\ pc' = "Loop"
               /\ UNCHANGED << remoteLog, cachedLog, activeSource, 
                               warnedFallback, lastProgress, jobDone, 
                               networkUp >>

StartFollow == /\ pc = "StartFollow"
               /\ IF jobDone /\ cachedLog = "cached"
                     THEN /\ mode' = "following"
                          /\ activeSource' = "cached"
                          /\ warnedFallback' = FALSE
                     ELSE /\ IF remoteLog = "remote" /\ networkUp
                                THEN /\ mode' = "following"
                                     /\ activeSource' = "remote"
                                     /\ warnedFallback' = FALSE
                                ELSE /\ IF cachedLog = "cached"
                                           THEN /\ mode' = "following"
                                                /\ activeSource' = "cached"
                                                /\ warnedFallback' = TRUE
                                           ELSE /\ TRUE
                                                /\ UNCHANGED << mode, 
                                                                activeSource, 
                                                                warnedFallback >>
               /\ pc' = "Loop"
               /\ UNCHANGED << remoteLog, cachedLog, lastProgress, jobDone, 
                               networkUp >>

StopFollow == /\ pc = "StopFollow"
              /\ mode' = "idle"
              /\ activeSource' = "none"
              /\ warnedFallback' = FALSE
              /\ pc' = "Loop"
              /\ UNCHANGED << remoteLog, cachedLog, lastProgress, jobDone, 
                              networkUp >>

ParseProgress == /\ pc = "ParseProgress"
                 /\ \E p \in 0..MaxProgress:
                      lastProgress' = p
                 /\ pc' = "Loop"
                 /\ UNCHANGED << mode, remoteLog, cachedLog, activeSource, 
                                 warnedFallback, jobDone, networkUp >>

MarkJobDone == /\ pc = "MarkJobDone"
               /\ jobDone' = TRUE
               /\ pc' = "Loop"
               /\ UNCHANGED << mode, remoteLog, cachedLog, activeSource, 
                               warnedFallback, lastProgress, networkUp >>

NetworkFlap == /\ pc = "NetworkFlap"
               /\ networkUp' = ~networkUp
               /\ pc' = "Loop"
               /\ UNCHANGED << mode, remoteLog, cachedLog, activeSource, 
                               warnedFallback, lastProgress, jobDone >>

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == pc = "Done" /\ UNCHANGED vars

Next == InitState \/ Loop \/ RemoteLogAppears \/ CacheLog \/ StartStream
           \/ StartFollow \/ StopFollow \/ ParseProgress \/ MarkJobDone
           \/ NetworkFlap
           \/ Terminating

Spec == Init /\ [][Next]_vars

Termination == <>(pc = "Done")

\* END TRANSLATION 

TypeInvariant ==
    /\ mode \in ModeSet
    /\ remoteLog \in LogStateSet
    /\ cachedLog \in LogStateSet
    /\ activeSource \in SourceSet
    /\ warnedFallback \in BOOLEAN
    /\ lastProgress \in ProgressRange
    /\ jobDone \in BOOLEAN
    /\ networkUp \in BOOLEAN

=============================================================================
