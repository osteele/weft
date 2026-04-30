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

Both target kinds now do the source → destination transition atomically
via `db.TransferJobLaunchID`:

- Move-to-existing → `campaign.SubmitJobsToInstanceForMove` (transfer at
  R2-submit time).
- Move-to-new → `campaign.LaunchOpts.TransferClaim=true` (transfer at
  instance-creation time inside `LaunchCampaign`).

The autopilot/rebalance exclusion and failure-path restore-to-source are
in place. The aspirational invariant
`SourceLaunchUnchangedWhileIntentOpen` is now hard for both target
kinds. Outstanding work (see "Future work" in
`specs/job-move.allium`): two-attempt overlap reconciler and caller-side
ack-waiting on the running-instance grace path.
