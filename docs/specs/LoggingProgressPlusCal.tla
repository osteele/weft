----------------------------- MODULE LoggingProgressPlusCal -----------------------------
EXTENDS TLC, Sequences

\* Models log files, streaming, caching, and progress parsing.

ModeSet == {"idle", "streaming", "following"}
LogStateSet == {"missing", "remote", "cached"}
SourceSet == {"none", "remote", "cached"}

(* --algorithm LoggingProgress
variables
    mode \in ModeSet,
    remoteLog \in LogStateSet,
    cachedLog \in LogStateSet,
    activeSource \in SourceSet,
    warnedFallback \in BOOLEAN,
    lastProgress \in Int,
    jobDone \in BOOLEAN,
    networkUp \in BOOLEAN;

begin
Init:
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
                with p \in 0..100 do
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

TypeInvariant ==
    /\ mode \in ModeSet
    /\ remoteLog \in LogStateSet
    /\ cachedLog \in LogStateSet
    /\ activeSource \in SourceSet
    /\ warnedFallback \in BOOLEAN
    /\ lastProgress \in Int
    /\ jobDone \in BOOLEAN
    /\ networkUp \in BOOLEAN

=============================================================================
