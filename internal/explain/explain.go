package explain

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/queueblock"
)

const InventoryDispatchReplanThreshold = 10 * time.Minute

type Evidence struct {
	Label string
	Value string
}

type Option struct {
	Label  string
	Detail string
}

type Explanation struct {
	JobID             int64
	State             string
	PrimaryReason     string
	Evidence          []Evidence
	Options           []Option
	SuggestedAction   string
	Confidence        string
	AutoReplanAllowed bool
}

type DispatchBlock struct {
	Kind            string
	Detail          string
	OccurredAt      time.Time
	FirstOccurredAt time.Time
	RetryCount      int
}

func ForJob(database *sql.DB, job *db.Job, now time.Time) Explanation {
	if now.IsZero() {
		now = time.Now()
	}
	if job == nil {
		return Explanation{}
	}
	display := queueblock.Display(job, nil)
	state := job.EffectiveStatus()
	reason := ""
	if display.Kind != "" {
		state = display.Status
		reason = display.Reason
	}
	x := Explanation{
		JobID:         job.ID,
		State:         state,
		PrimaryReason: reason,
		Confidence:    "medium",
	}
	if job.TargetDisplay() != "" {
		x.Evidence = append(x.Evidence, Evidence{Label: "target", Value: job.TargetDisplay()})
	}
	if job.QueuedAt > 0 && job.EffectiveStatus() == db.StatusQueued {
		x.Evidence = append(x.Evidence, Evidence{Label: "queued", Value: shortAge(now, time.Unix(job.QueuedAt, 0)) + " ago"})
	}
	if reason != "" {
		x.Evidence = append(x.Evidence, Evidence{Label: "blocker", Value: reason})
	}
	appendPlacementEvidence(database, job, &x)
	if block, ok := LatestInventoryDispatchBlock(database, job, now); ok {
		x.Confidence = "high"
		x.PrimaryReason = annotateDispatchBlock(block, now)
		kind := queueblock.ReasonKind(block.Detail)
		if display.Kind == "" {
			x.State = kind
		}
		x.Evidence = append(x.Evidence, Evidence{Label: "dispatch", Value: fmt.Sprintf("%s %s ago", block.Kind, shortAge(now, block.OccurredAt))})
		if block.RetryCount > 1 {
			x.Evidence = append(x.Evidence, Evidence{Label: "retries", Value: fmt.Sprintf("%d consecutive attempts", block.RetryCount)})
		}
		startedAt := block.FirstOccurredAt
		if startedAt.IsZero() {
			startedAt = block.OccurredAt
		}
		age := now.Sub(startedAt)
		x.Options = append(x.Options,
			Option{Label: "wait", Detail: "keep retrying dispatch on " + job.TargetDisplay()},
			Option{Label: "replan", Detail: "return the job to the unplaced pool for host or rental placement"},
		)
		if age >= InventoryDispatchReplanThreshold {
			x.SuggestedAction = fmt.Sprintf("replan: dispatch has been waiting for %s", shortDuration(age))
			x.AutoReplanAllowed = true
		} else {
			x.SuggestedAction = fmt.Sprintf("wait: auto-replan threshold is %s", InventoryDispatchReplanThreshold)
		}
		return x
	}
	if result := blockreason.Resolve(job, blockreason.Options{Compact: true}); result.Blocked {
		x.State = string(result.Kind)
		x.PrimaryReason = blockreason.DisplayReasonForKind(result.Kind, result.Reason)
		x.Confidence = "high"
		var producerWaitChain []queueblock.ProducerWait
		if chain := queueblock.TraceWaitingOnProducer(database, job); len(chain) > 0 {
			producerWaitChain = chain
			x.PrimaryReason = chain[0].Reason()
			appendProducerWaitEvidence(&x, chain)
		}
		if result.Source == blockreason.SourcePlacement {
			x.Evidence = append(x.Evidence, Evidence{Label: "placement", Value: result.Reason})
			if s := blockreason.ForJob(job); s.IsPlacementFailure() {
				for _, line := range s.DetailLines() {
					x.Evidence = append(x.Evidence, Evidence{Label: "avenue", Value: line})
				}
			}
		}
		x.Options = append(x.Options, Option{Label: "wait", Detail: "let the placement condition clear"})
		if job.TargetKind() != db.JobTargetUnplaced {
			x.Options = append(x.Options, Option{Label: "replan", Detail: "return the job to the unplaced pool"})
		}
		if len(producerWaitChain) > 1 {
			x.SuggestedAction = "inspect root producer"
		} else {
			x.SuggestedAction = "inspect blocker"
		}
		return x
	}
	if display.Kind != "" {
		x.Options = append(x.Options, Option{Label: "wait", Detail: "let the current queue or placement condition clear"})
		if job.TargetKind() != db.JobTargetUnplaced {
			x.Options = append(x.Options, Option{Label: "replan", Detail: "return the job to the unplaced pool"})
		}
		x.SuggestedAction = "inspect blocker"
		return x
	}
	switch job.EffectiveStatus() {
	case db.StatusQueued:
		x.PrimaryReason = queuedReason(job)
		x.SuggestedAction = queuedSuggestedAction(job)
	case db.StatusRunning:
		x.PrimaryReason = "job is running"
		x.SuggestedAction = "monitor progress"
	case db.StatusFailed, db.StatusDead:
		x.PrimaryReason = firstNonEmpty(job.FailureReason, job.ErrorMessage, "job failed")
		if latestLaunchTerminatedWith(database, job, db.TerminationReasonDiskFull) {
			x.SuggestedAction = "inspect instance disk report or retry"
		} else {
			x.SuggestedAction = "inspect log or retry"
		}
	default:
		x.PrimaryReason = "no blocker detected"
		x.SuggestedAction = "none"
	}
	return x
}

func latestLaunchTerminatedWith(database *sql.DB, job *db.Job, reason string) bool {
	if database == nil || job == nil || job.LaunchID == nil {
		return false
	}
	launch, err := db.GetLaunch(database, *job.LaunchID)
	if err != nil || launch == nil {
		return false
	}
	return launch.TerminationReason == reason
}

func appendProducerWaitEvidence(x *Explanation, chain []queueblock.ProducerWait) {
	if x == nil || len(chain) == 0 {
		return
	}
	if len(chain) == 1 {
		return
	}
	for i := 1; i < len(chain); i++ {
		label := producerWaitEvidenceLabel("producer", chain[i])
		if i == len(chain)-1 {
			label = producerWaitEvidenceLabel("root", chain[i])
		}
		x.Evidence = append(x.Evidence, Evidence{Label: label, Value: chain[i].Reason()})
	}
	root := chain[len(chain)-1]
	x.Options = append(x.Options, Option{
		Label:  "root",
		Detail: "inspect " + ids.FormatJobID(root.ProducerID),
	})
}

func producerWaitEvidenceLabel(prefix string, wait queueblock.ProducerWait) string {
	if queueblock.ReasonKind(wait.Reason()) == queueblock.KindWaiting {
		return prefix + " wait"
	}
	return prefix + " blocker"
}

// appendPlacementEvidence adds a "placed" line explaining why a rental-instance
// job is on its instance (reused vs launched, and whether it is queued behind
// other jobs on a single-GPU box). The database read is skipped when database
// is nil so TUI render paths that pass nil pay no cost.
func appendPlacementEvidence(database *sql.DB, job *db.Job, x *Explanation) {
	if database == nil || job == nil || job.TargetKind() != db.JobTargetRentalInstance {
		return
	}
	decision, err := db.LatestPlacementDecisionForJob(database, job.ID)
	if err != nil || decision == nil {
		return
	}
	if summary := placementSummary(decision); summary != "" {
		x.Evidence = append(x.Evidence, Evidence{Label: "placed", Value: summary})
	}
	if len(decision.Details.Rejected) > 0 {
		x.Evidence = append(x.Evidence, Evidence{Label: "rejected", Value: strings.Join(decision.Details.Rejected, "; ")})
	}
}

func placementSummary(decision *db.JobPlacementDecision) string {
	det := decision.Details
	head := det.Instance
	if head == "" {
		head = decision.SelectedTarget
	}
	switch {
	case det.GPU != "" && det.CostPerHour != "":
		head = fmt.Sprintf("%s (%s, %s)", head, det.GPU, det.CostPerHour)
	case det.GPU != "":
		head = fmt.Sprintf("%s (%s)", head, det.GPU)
	}
	verb := "placed on"
	switch decision.SelectedKind {
	case "reuse-instance":
		verb = "reused"
	case "launch-instance", "cloud-offer":
		verb = "launched"
	}
	summary := strings.TrimSpace(verb + " " + head)
	if det.Why != "" {
		summary += " — " + det.Why
	}
	return summary
}

func queuedReason(job *db.Job) string {
	if job == nil {
		return "job is queued"
	}
	switch job.TargetKind() {
	case db.JobTargetUnplaced:
		return "awaiting placement"
	case db.JobTargetRentalInstance:
		return "waiting for assigned instance to start the job"
	case db.JobTargetInventoryHost:
		if target := strings.TrimSpace(job.TargetDisplay()); target != "" {
			return "waiting in queue on " + target
		}
		return "waiting in host queue"
	case db.JobTargetExternal:
		return "waiting for external executor"
	default:
		return "job is queued"
	}
}

func queuedSuggestedAction(job *db.Job) string {
	if job == nil {
		return "wait"
	}
	switch job.TargetKind() {
	case db.JobTargetUnplaced:
		return "wait for placement"
	case db.JobTargetRentalInstance:
		return "wait for instance startup or inspect instance"
	case db.JobTargetInventoryHost:
		return "wait for queue slot"
	case db.JobTargetExternal:
		return "inspect external executor"
	default:
		return "wait"
	}
}

func LatestInventoryDispatchBlock(database *sql.DB, job *db.Job, now time.Time) (DispatchBlock, bool) {
	if database == nil || job == nil || !job.HasInventoryHost() || job.EffectiveStatus() != db.StatusQueued {
		return DispatchBlock{}, false
	}
	floor := job.QueuedAt
	if floor <= 0 {
		floor = job.CreatedAt
	}
	return latestDispatchBlock(database, job.ID, floor, now)
}

func latestDispatchBlock(database *sql.DB, jobID int64, floor int64, now time.Time) (DispatchBlock, bool) {
	rows, err := database.Query(`SELECT occurred_at, event_kind, COALESCE(detail, '')
		FROM lifecycle_events
		WHERE job_id = ?
		  AND event_kind IN (?, ?, ?)
		ORDER BY occurred_at DESC, id DESC`,
		jobID, db.EventQueueDispatchFailed, db.EventQueueDispatchOK, db.EventQueueDispatchDeferred)
	if err != nil {
		return DispatchBlock{}, false
	}
	defer rows.Close()

	var block DispatchBlock
	closed := false
	var latestOK int64
	for rows.Next() {
		var occurredAt int64
		var kind string
		var detail string
		if err := rows.Scan(&occurredAt, &kind, &detail); err != nil {
			continue
		}
		if floor > 0 && occurredAt < floor {
			continue
		}
		if kind == db.EventQueueDispatchOK {
			if latestOK == 0 || occurredAt > latestOK {
				latestOK = occurredAt
			}
			if block.Detail != "" {
				closed = true
			}
			continue
		}
		if closed {
			continue
		}
		detail = strings.TrimSpace(detail)
		if detail == "" {
			continue
		}
		if latestOK > 0 && latestOK >= occurredAt && !dispatchFailurePersistsAfterOK(detail) {
			continue
		}
		if block.Detail == "" {
			block = DispatchBlock{
				Kind:            kind,
				Detail:          detail,
				OccurredAt:      time.Unix(occurredAt, 0),
				FirstOccurredAt: time.Unix(occurredAt, 0),
				RetryCount:      1,
			}
			continue
		}
		if detail == block.Detail {
			block.FirstOccurredAt = time.Unix(occurredAt, 0)
			block.RetryCount++
		} else {
			closed = true
		}
	}
	if block.Detail == "" {
		return DispatchBlock{}, false
	}
	return block, true
}

func dispatchFailurePersistsAfterOK(detail string) bool {
	return strings.TrimSpace(detail) == db.FailureReasonR2IsolatedSourceFetchFailed
}

func SummaryLine(x Explanation) string {
	if strings.TrimSpace(x.PrimaryReason) == "" {
		return ""
	}
	if x.State != "" {
		return x.State + ": " + x.PrimaryReason
	}
	return x.PrimaryReason
}

func DetailLines(x Explanation) []string {
	var lines []string
	if x.PrimaryReason != "" {
		lines = append(lines, "Why: "+x.PrimaryReason)
	}
	for _, ev := range x.Evidence {
		if ev.Label == "" || ev.Value == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %s", ev.Label, ev.Value))
	}
	for _, opt := range x.Options {
		if opt.Label == "" || opt.Detail == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("option %s: %s", opt.Label, opt.Detail))
	}
	if x.SuggestedAction != "" {
		lines = append(lines, "suggested: "+x.SuggestedAction)
	}
	return lines
}

func DiagnoseText(x Explanation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Diagnosis for %s\n", ids.FormatJobID(x.JobID))
	if x.State != "" {
		fmt.Fprintf(&b, "State:      %s\n", x.State)
	}
	if x.PrimaryReason != "" {
		fmt.Fprintf(&b, "Why:        %s\n", x.PrimaryReason)
	}
	for _, ev := range x.Evidence {
		fmt.Fprintf(&b, "Evidence:   %s: %s\n", ev.Label, ev.Value)
	}
	if len(x.Options) > 0 {
		fmt.Fprintln(&b, "Options:")
		for _, opt := range x.Options {
			fmt.Fprintf(&b, "  %-8s %s\n", opt.Label, opt.Detail)
		}
	}
	if x.SuggestedAction != "" {
		fmt.Fprintf(&b, "Suggested:  %s\n", x.SuggestedAction)
	}
	if x.Confidence != "" {
		fmt.Fprintf(&b, "Confidence: %s\n", x.Confidence)
	}
	return strings.TrimRight(b.String(), "\n")
}

func annotateDispatchBlock(block DispatchBlock, now time.Time) string {
	reason := block.Detail
	if !block.OccurredAt.IsZero() {
		reason = fmt.Sprintf("[%s ago] %s", shortAge(now, block.OccurredAt), reason)
	}
	if block.RetryCount > 1 {
		reason = fmt.Sprintf("%s (retry #%d)", reason, block.RetryCount)
	}
	return reason
}

func shortAge(now, then time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	if then.IsZero() {
		return "unknown"
	}
	return shortDuration(now.Sub(then))
}

func shortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		days := int(d.Hours()) / 24
		hours := int(d.Hours()) % 24
		if hours > 0 && days < 10 {
			return fmt.Sprintf("%dd%dh", days, hours)
		}
		return fmt.Sprintf("%dd", days)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
