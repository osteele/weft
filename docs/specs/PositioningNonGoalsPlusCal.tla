-------------------------- MODULE PositioningNonGoalsPlusCal --------------------------
EXTENDS TLC

\* This module captures explicit non-goals. It asserts what the system does NOT do.

ResourceScheduling == FALSE
MultiNodeJobs == FALSE
CentralController == FALSE
AutomaticHostSelection == FALSE
MultiUserArbitration == FALSE

NonGoalsInvariant ==
    /\ ResourceScheduling = FALSE
    /\ MultiNodeJobs = FALSE
    /\ CentralController = FALSE
    /\ AutomaticHostSelection = FALSE
    /\ MultiUserArbitration = FALSE

=============================================================================
