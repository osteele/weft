# Job Move Protocol

The model lives in [`specs/job-move.allium`](../../specs/job-move.allium).
That spec defines:

- `MoveIntent` entity, `MoveTargetKind` (existing | new), `MoveIntentState`
  (open → confirmed | canceled | obsoleted)
- Rules: `UserMovesQueuedJob`, `SecondMoveOnSameJobRejected`,
  `MoveTargetSameAsSourceRejected`, `MoveToNewLaunchAttachesIntent`,
  `ConfirmMoveOnSuccess`, `CancelMoveOnFailure`,
  `ObsoleteMoveOnSourceCompletion`
- Invariants: `AtMostOneOpenIntentPerJob`,
  `AutopilotIgnoresMovingJobs`, `SourceLaunchUnchangedWhileIntentOpen`
  (aspirational — see spec), `MoveTargetExistingHasInstance`,
  `ResolvedIntentHasResolutionTime`

## Implementation map

- Migration + storage: `internal/db/move_intents.go`,
  `internal/db/migrations.go`.
- Move flow: `internal/orchestration/move.go` (`ExecuteOption`,
  `openMoveIntent`, `tryRestoreJobToSource`).
- Autopilot exclusion: `internal/orchestration/autopilot.go` and
  `internal/orchestration/rebalance.go` (both filter by
  `db.JobIDsWithOpenMoveIntents`).
- Tests: `internal/db/move_intents_test.go`,
  `internal/orchestration/move_test.go`,
  `internal/orchestration/autopilot_test.go` (regression:
  `TestRunGroupedAutoPilotPass_ExcludesJobsWithOpenMoveIntent`).

## Status

The intent table, autopilot/rebalance exclusion, and failure-path
restore-to-source are in place. Today's `ExecuteOption` still mutates
`Job.cloud_instance` mid-move (unplace then claim) and relies on
`tryRestoreJobToSource` as the rollback. The next refinement (see the
"Future work" section in `specs/job-move.allium`) makes the move flow
genuinely speculative — no source mutation until the destination's
agent acknowledges — at which point `SourceLaunchUnchangedWhileIntentOpen`
becomes a hard invariant.
