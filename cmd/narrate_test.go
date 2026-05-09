package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/narrate"
)

func TestLoadUnprocessedCountsIncludesTerminalProjects(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Now().Unix()

	completedAugur, err := db.RecordQueued(database, "cool30", "/tmp/augur", "echo ok", "completed")
	if err != nil {
		t.Fatalf("record completed augur: %v", err)
	}
	if err := db.SetJobProject(database, completedAugur, "augur"); err != nil {
		t.Fatalf("set completed augur project: %v", err)
	}
	exitZero := 0
	if err := db.CloseAttempt(database, completedAugur, db.StatusCompleted, &exitZero, now); err != nil {
		t.Fatalf("close completed augur: %v", err)
	}

	completedUnset, err := db.RecordQueued(database, "cool30", "/tmp", "echo ok", "completed unset")
	if err != nil {
		t.Fatalf("record completed unset: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET project = '' WHERE id = ?`, completedUnset); err != nil {
		t.Fatalf("clear completed unset project: %v", err)
	}
	if err := db.CloseAttempt(database, completedUnset, db.StatusCompleted, &exitZero, now); err != nil {
		t.Fatalf("close completed unset: %v", err)
	}

	failedAugur, err := db.RecordQueued(database, "cool30", "/tmp/augur", "false", "failed")
	if err != nil {
		t.Fatalf("record failed augur: %v", err)
	}
	if err := db.SetJobProject(database, failedAugur, "augur"); err != nil {
		t.Fatalf("set failed augur project: %v", err)
	}
	exitOne := 1
	if err := db.CloseAttempt(database, failedAugur, db.StatusFailed, &exitOne, now); err != nil {
		t.Fatalf("close failed augur: %v", err)
	}

	completedBad, err := db.RecordQueued(database, "cool30", "/tmp/bad-project", "false", "completed bad")
	if err != nil {
		t.Fatalf("record completed bad: %v", err)
	}
	if err := db.SetJobProject(database, completedBad, "bad-project"); err != nil {
		t.Fatalf("set completed bad project: %v", err)
	}
	if err := db.CloseAttempt(database, completedBad, db.StatusCompleted, &exitOne, now); err != nil {
		t.Fatalf("close completed bad: %v", err)
	}

	killedJob, err := db.RecordQueued(database, "cool30", "/tmp/ignored", "sleep 10", "killed")
	if err != nil {
		t.Fatalf("record killed job: %v", err)
	}
	if err := db.SetJobProject(database, killedJob, "ignored-project"); err != nil {
		t.Fatalf("set killed project: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`, db.StatusKilled, now, killedJob); err != nil {
		t.Fatalf("mark killed: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, db.StatusKilled, killedJob); err != nil {
		t.Fatalf("request killed: %v", err)
	}

	canceledJob, err := db.RecordQueued(database, "cool30", "/tmp/ignored", "sleep 10", "canceled")
	if err != nil {
		t.Fatalf("record canceled job: %v", err)
	}
	if err := db.SetJobProject(database, canceledJob, "ignored-project"); err != nil {
		t.Fatalf("set canceled project: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`, db.StatusCanceled, now, canceledJob); err != nil {
		t.Fatalf("mark canceled: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, db.StatusCanceled, canceledJob); err != nil {
		t.Fatalf("request canceled: %v", err)
	}

	processedFailed, err := db.RecordQueued(database, "cool30", "/tmp/processed", "false", "processed failed")
	if err != nil {
		t.Fatalf("record processed failed: %v", err)
	}
	if err := db.SetJobProject(database, processedFailed, "processed-project"); err != nil {
		t.Fatalf("set processed failed project: %v", err)
	}
	if err := db.CloseAttempt(database, processedFailed, db.StatusFailed, &exitOne, now); err != nil {
		t.Fatalf("close processed failed: %v", err)
	}
	if err := db.AddJobTag(database, processedFailed, db.ProcessedTag); err != nil {
		t.Fatalf("tag processed failed: %v", err)
	}

	processedCompleted, err := db.RecordQueued(database, "cool30", "/tmp/processed", "echo ok", "processed completed")
	if err != nil {
		t.Fatalf("record processed completed: %v", err)
	}
	if err := db.SetJobProject(database, processedCompleted, "processed-project"); err != nil {
		t.Fatalf("set processed completed project: %v", err)
	}
	if err := db.CloseAttempt(database, processedCompleted, db.StatusCompleted, &exitZero, now); err != nil {
		t.Fatalf("close processed completed: %v", err)
	}
	if err := db.AddJobTag(database, processedCompleted, db.ProcessedTag); err != nil {
		t.Fatalf("tag processed completed: %v", err)
	}

	counts, err := loadUnprocessedCounts(database, "")
	if err != nil {
		t.Fatalf("loadUnprocessedCounts: %v", err)
	}
	if counts.Completed != 2 {
		t.Fatalf("Completed = %d, want 2", counts.Completed)
	}
	if counts.Failed != 2 {
		t.Fatalf("Failed = %d, want 2", counts.Failed)
	}
	if got := counts.CompletedProjects; len(got) != 1 || got[0] != "augur" {
		t.Fatalf("CompletedProjects = %v, want [augur]", got)
	}
	if got := counts.FailedProjects; len(got) != 2 || got[0] != "augur" || got[1] != "bad-project" {
		t.Fatalf("FailedProjects = %v, want [augur bad-project]", got)
	}
}

func TestNarrateSlackPostingIsRateLimited(t *testing.T) {
	var posted []string
	base := time.Unix(1710000000, 0)
	now := base
	r := &narrateRunner{
		slackEnabled:     true,
		slackMinInterval: 5 * time.Minute,
		slackPost: func(message string) error {
			posted = append(posted, message)
			return nil
		},
		now: func() time.Time { return now },
	}
	statusLine := narrate.StatusLine{
		Now:             base,
		ActiveInstances: 1,
		AutopilotState:  "idle",
	}

	r.maybePostSlack(statusLine, "first", true)
	now = base.Add(2 * time.Minute)
	r.maybePostSlack(statusLine, "second", true)
	now = base.Add(5 * time.Minute)
	r.maybePostSlack(statusLine, "third", true)

	if len(posted) != 2 {
		t.Fatalf("posted %d messages, want 2: %v", len(posted), posted)
	}
	if posted[0] == posted[1] {
		t.Fatalf("expected distinct messages, got %q", posted[0])
	}
}

func TestNarrateSlackPostingSkipsEmptyUnchangedEntry(t *testing.T) {
	var posted []string
	r := &narrateRunner{
		slackEnabled:     true,
		slackMinInterval: 5 * time.Minute,
		slackPost: func(message string) error {
			posted = append(posted, message)
			return nil
		},
		now: time.Now,
	}

	r.maybePostSlack(narrate.StatusLine{Now: time.Unix(1710000000, 0)}, "", false)
	if len(posted) != 0 {
		t.Fatalf("posted %d messages, want 0", len(posted))
	}
}

func TestFormatNarrateSlackMessageStripsANSI(t *testing.T) {
	msg := formatNarrateSlackMessage(narrate.StatusLine{
		Now:                  time.Unix(1710000000, 0),
		UnprocessedCompleted: 1,
		CompletedProjects:    []string{"augur"},
		AutopilotState:       "idle",
	}, "done", 100)
	if strings.Contains(msg, "\x1b[") {
		t.Fatalf("Slack message contains ANSI escapes: %q", msg)
	}
	for _, want := range []string{"1 job completed", "augur", "done"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("Slack message missing %q: %q", want, msg)
		}
	}
}

func TestValidateNarrateSlackConfigRequiresWebhookWhenEnabled(t *testing.T) {
	err := validateNarrateSlackConfig(true, "")
	if err == nil {
		t.Fatal("expected missing webhook to fail when Slack is enabled")
	}
	if !strings.Contains(err.Error(), "no webhook") {
		t.Fatalf("error = %q, want missing webhook guidance", err)
	}
}

func TestValidateNarrateSlackConfigAllowsMissingWebhookWhenDisabled(t *testing.T) {
	if err := validateNarrateSlackConfig(false, ""); err != nil {
		t.Fatalf("disabled Slack should not require webhook: %v", err)
	}
}

func TestValidateNarrateSlackConfigAllowsConfiguredWebhook(t *testing.T) {
	if err := validateNarrateSlackConfig(true, "https://hooks.slack.example/test"); err != nil {
		t.Fatalf("configured webhook should be accepted: %v", err)
	}
}

func TestNarrateStartupOverviewRecordsStatusWithoutSlack(t *testing.T) {
	database := db.SetupTestDB(t)
	sess, err := narrate.NewSession(narrate.SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var posted []string
	base := time.Unix(1710000000, 0)
	r := &narrateRunner{
		session:          sess,
		database:         database,
		slackEnabled:     true,
		slackMinInterval: 5 * time.Minute,
		slackPost: func(message string) error {
			posted = append(posted, message)
			return nil
		},
		now: func() time.Time { return base },
	}
	snap := &narrate.Snapshot{
		Time:      base,
		Jobs:      map[int64]narrate.JobView{},
		Instances: map[int64]narrate.InstanceView{},
		Autopilot: narrate.AutopilotView{State: "idle"},
	}

	var out bytes.Buffer
	if err := r.emitStartupOverview(&out, snap); err != nil {
		t.Fatalf("emitStartupOverview: %v", err)
	}
	if !strings.Contains(out.String(), "no rentals") || !strings.Contains(out.String(), "autopilot:idle") {
		t.Fatalf("startup overview missing status: %q", out.String())
	}
	if len(posted) != 0 {
		t.Fatalf("startup overview posted to Slack: %v", posted)
	}

	statusLine := narrate.BuildStatusLine(snap, 0, narrate.UnprocessedCounts{})
	if changed := sess.UpdateStatus(statusLine); changed {
		t.Fatal("startup overview should record status to avoid duplicate unchanged entry")
	}
}

func TestShouldSkipNoTransitionTickIgnoresStatusOnlyUpdates(t *testing.T) {
	prev := &narrate.Snapshot{}
	if !shouldSkipNoTransitionTick(nil, narrate.Delta{}, prev, false) {
		t.Fatal("no-transition tick should be skipped even if the status line changed")
	}
}

func TestShouldSkipNoTransitionTickKeepsInitialAndTransitionTicks(t *testing.T) {
	if shouldSkipNoTransitionTick(nil, narrate.Delta{}, nil, false) {
		t.Fatal("initial tick should not be skipped")
	}
	if shouldSkipNoTransitionTick(nil, narrate.Delta{}, &narrate.Snapshot{}, true) {
		t.Fatal("one-shot tick should not be skipped")
	}
	if shouldSkipNoTransitionTick([]db.LifecycleEvent{{EventKind: "job.started"}}, narrate.Delta{}, &narrate.Snapshot{}, false) {
		t.Fatal("lifecycle-event tick should not be skipped")
	}
	if shouldSkipNoTransitionTick(nil, narrate.Delta{JobAdded: []narrate.JobView{{ID: 1}}}, &narrate.Snapshot{}, false) {
		t.Fatal("snapshot-transition tick should not be skipped")
	}
}

func TestNarrateAddsTerminalJobsCompletedBetweenTicks(t *testing.T) {
	database := db.SetupTestDB(t)
	prevTime := time.Now().Add(-2 * time.Second)
	jobID, err := db.RecordQueued(database, "cool30", "/tmp/quick", "echo ok", "quick")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}
	if err := db.SetJobProject(database, jobID, "quick-project"); err != nil {
		t.Fatalf("set project: %v", err)
	}
	exitZero := 0
	if err := db.CloseAttempt(database, jobID, db.StatusCompleted, &exitZero, time.Now().Unix()); err != nil {
		t.Fatalf("close attempt: %v", err)
	}

	r := &narrateRunner{
		database: database,
		opts:     narrate.SnapshotOptions{},
	}
	delta := narrate.Delta{}
	prev := &narrate.Snapshot{
		Time:      prevTime,
		Jobs:      map[int64]narrate.JobView{},
		Instances: map[int64]narrate.InstanceView{},
	}
	if err := r.addRecentTerminalJobs(&delta, prev); err != nil {
		t.Fatalf("addRecentTerminalJobs: %v", err)
	}
	if len(delta.JobFinished) != 1 {
		t.Fatalf("JobFinished len = %d, want 1: %#v", len(delta.JobFinished), delta.JobFinished)
	}
	if delta.JobFinished[0].ID != jobID || delta.JobFinished[0].Status != db.StatusCompleted {
		t.Fatalf("unexpected terminal job: %+v", delta.JobFinished[0])
	}
}

func TestNarrateLoadsLifecycleEventsAfterCursor(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{EventKind: db.EventRelaunchEligible}); err != nil {
		t.Fatalf("insert first event: %v", err)
	}
	cursor, err := db.LatestLifecycleEventID(database)
	if err != nil {
		t.Fatalf("LatestLifecycleEventID: %v", err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{EventKind: db.EventRelaunchLaunchSuccess, LaunchID: 44}); err != nil {
		t.Fatalf("insert second event: %v", err)
	}
	r := &narrateRunner{database: database, lifecycleCursor: cursor}
	events, nextCursor, err := r.loadLifecycleEvents()
	if err != nil {
		t.Fatalf("loadLifecycleEvents: %v", err)
	}
	if len(events) != 1 || events[0].EventKind != db.EventRelaunchLaunchSuccess {
		t.Fatalf("events = %#v, want one launch success", events)
	}
	if nextCursor <= cursor {
		t.Fatalf("next cursor = %d, want > %d", nextCursor, cursor)
	}
}

func TestNarrateFiltersDispatchOKEventsButAdvancesCursor(t *testing.T) {
	database := db.SetupTestDB(t)
	cursor, err := db.LatestLifecycleEventID(database)
	if err != nil {
		t.Fatalf("LatestLifecycleEventID: %v", err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{EventKind: db.EventQueueDispatchOK, JobID: 10}); err != nil {
		t.Fatalf("insert dispatch ok: %v", err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{EventKind: db.EventRelaunchPassSummary, Detail: "eligible=0 launched=0 skipped=2 errors=0 blocked=\"\""}); err != nil {
		t.Fatalf("insert pass summary: %v", err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{EventKind: db.EventRelaunchSkippedBackoff, JobID: 12, Detail: "backoff 10m remaining"}); err != nil {
		t.Fatalf("insert backoff: %v", err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{EventKind: db.EventQueueDispatchFailed, JobID: 11, Detail: "sync failed"}); err != nil {
		t.Fatalf("insert dispatch failed: %v", err)
	}
	r := &narrateRunner{database: database, lifecycleCursor: cursor}
	events, nextCursor, err := r.loadLifecycleEvents()
	if err != nil {
		t.Fatalf("loadLifecycleEvents: %v", err)
	}
	if len(events) != 1 || events[0].EventKind != db.EventQueueDispatchFailed {
		t.Fatalf("events = %#v, want only dispatch failed", events)
	}
	latest, err := db.LatestLifecycleEventID(database)
	if err != nil {
		t.Fatalf("LatestLifecycleEventID: %v", err)
	}
	if nextCursor != latest {
		t.Fatalf("next cursor = %d, want latest %d", nextCursor, latest)
	}
}
