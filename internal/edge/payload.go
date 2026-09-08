package edge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// JobControlAction is one operation the hub may perform on an existing job.
type JobControlAction string

const (
	ControlCancel          JobControlAction = "cancel"
	ControlKill            JobControlAction = "kill"
	ControlPause           JobControlAction = "pause"
	ControlResume          JobControlAction = "resume"
	ControlUnpause         JobControlAction = "unpause"
	ControlRestart         JobControlAction = "restart"
	ControlMarkProcessed   JobControlAction = "mark-processed"
	ControlMarkUnprocessed JobControlAction = "mark-unprocessed"
	ControlEdit            JobControlAction = "edit"
)

// WeftJobControlPayload is the payload for KindWeftJobControl.
//
// RequestID identifies the instruction independently of its delivery nonce.
// Two deliveries of one instruction therefore have identical content and
// collapse, while two deliberate requests receive different IDs even when
// they name the same job and action.
type WeftJobControlPayload struct {
	RequestID       string            `json:"request_id"`
	JobID           int64             `json:"job_id"`
	Action          JobControlAction  `json:"action"`
	Flags           map[string]string `json:"flags,omitempty"`
	SpendCeilingUSD float64           `json:"spend_ceiling_usd,omitempty"`
}

var validJobControlActions = map[JobControlAction]bool{
	ControlCancel: true, ControlKill: true, ControlPause: true,
	ControlResume: true, ControlUnpause: true, ControlRestart: true,
	ControlMarkProcessed: true, ControlMarkUnprocessed: true, ControlEdit: true,
}

// ParseWeftJobControlPayload decodes a verified control request.
func ParseWeftJobControlPayload(data []byte) (*WeftJobControlPayload, *Refusal) {
	var p WeftJobControlPayload
	if err := decodePayloadStrict(data, &p); err != nil {
		return nil, refuse(ReasonMalformed, "payload is not a valid weft job-control request: %v", err)
	}
	if strings.TrimSpace(p.RequestID) == "" {
		return nil, refuse(ReasonMalformed, "weft job-control request has no request_id")
	}
	if p.JobID <= 0 {
		return nil, refuse(ReasonMalformed, "weft job-control request names invalid job id %d", p.JobID)
	}
	if !validJobControlActions[p.Action] {
		return nil, refuse(ReasonMalformed, "weft job-control request names unknown action %q", p.Action)
	}
	if p.SpendCeilingUSD < 0 {
		return nil, refuse(ReasonMalformed, "weft job-control request declares negative spend ceiling %.2f", p.SpendCeilingUSD)
	}
	if p.Action != ControlRestart && p.Action != ControlEdit && p.SpendCeilingUSD != 0 {
		return nil, refuse(ReasonMalformed, "job-control action %q has no spend authority field", p.Action)
	}
	if p.Action == ControlEdit {
		declared, present, err := editSpendCeiling(p.Flags)
		if err != nil {
			return nil, refuse(ReasonMalformed, "job-control edit has invalid --max-spend: %v", err)
		}
		if present && declared != p.SpendCeilingUSD {
			return nil, refuse(ReasonMalformed,
				"job-control edit --max-spend declares %.2f but spend authority field declares %.2f",
				declared, p.SpendCeilingUSD)
		}
		if !present && p.SpendCeilingUSD != 0 {
			return nil, refuse(ReasonMalformed, "job-control edit has spend authority without --max-spend")
		}
	}
	if len(p.Flags) > 0 && p.Action != ControlRestart && p.Action != ControlEdit {
		return nil, refuse(ReasonMalformed, "job-control action %q does not accept flags", p.Action)
	}
	allowedFlags := jobControlFlags[p.Action]
	for name := range p.Flags {
		if strings.TrimSpace(name) == "" {
			return nil, refuse(ReasonMalformed, "job-control request contains an empty flag name")
		}
		if !allowedFlags[name] {
			return nil, refuse(ReasonMalformed, "job-control action %q does not accept flag --%s", p.Action, name)
		}
	}
	return &p, nil
}

func editSpendCeiling(flags map[string]string) (float64, bool, error) {
	raw, present := flags["max-spend"]
	if !present {
		return 0, false, nil
	}
	value := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "$"))
	dollars, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(dollars) || math.IsInf(dollars, 0) || dollars < 0 {
		return 0, true, fmt.Errorf("must be a non-negative dollar amount")
	}
	cents := math.Round(dollars * 100)
	if math.Abs(dollars*100-cents) > 1e-6 {
		return 0, true, fmt.Errorf("supports at most two decimal places")
	}
	return cents / 100, true, nil
}

var jobControlFlags = map[JobControlAction]map[string]bool{
	ControlRestart: {
		"gpu": true, "gpu-class": true, "provider": true, "gpu-mem": true,
		"disk": true, "runtime-disk": true, "disk-max": true, "min-survival": true,
		"gpu-mem-strict": true, "from-scratch": true, "checkpointed": true,
		"wait": true, "no-wait": true,
	},
	ControlEdit: {
		"message": true, "project": true, "directory": true, "command": true,
		"env": true, "clear-env": true, "tag": true, "remove-tag": true,
		"clear-tags": true, "depends-on": true, "depends-on-any": true,
		"clear-depends": true, "status": true, "retry": true, "gpu-class": true,
		"gpu-mem": true, "min-survival": true, "max-hourly-rate": true,
		"max-spend": true, "max-time": true, "grace-period": true,
		"provider": true, "runpod-cloud-type": true, "input": true,
		"clear-inputs": true, "needs": true, "clear-needs": true,
	},
}

// EncodeWeftJobControlPayload serializes a control request for signing.
func EncodeWeftJobControlPayload(p WeftJobControlPayload) ([]byte, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encode weft job-control payload: %w", err)
	}
	return data, nil
}

// BugReportAction distinguishes a new report from a note on an existing bug.
type BugReportAction string

const (
	BugReportCreate BugReportAction = "report"
	BugReportNote   BugReportAction = "note"
)

// WeftBugReportPayload is the payload for KindWeftBugReport.
//
// ReportID is generated for the fact itself. It keeps two reports with equal
// fields from producing equal payload bytes, while an exact retry retains the
// same identity and is content-idempotent.
type WeftBugReportPayload struct {
	ReportID    string          `json:"report_id"`
	Action      BugReportAction `json:"action"`
	BugID       string          `json:"bug_id,omitempty"`
	Title       string          `json:"title,omitempty"`
	Kind        string          `json:"kind,omitempty"`
	Scope       string          `json:"scope,omitempty"`
	Likelihood  string          `json:"likelihood,omitempty"`
	Severity    string          `json:"severity,omitempty"`
	Fingerprint string          `json:"fingerprint,omitempty"`
	JobID       *int64          `json:"job_id,omitempty"`
	Host        string          `json:"host,omitempty"`
	Summary     string          `json:"summary,omitempty"`
	Detail      string          `json:"detail,omitempty"`
	Note        string          `json:"note,omitempty"`
}

// ParseWeftBugReportPayload decodes a verified fact payload.
func ParseWeftBugReportPayload(data []byte) (*WeftBugReportPayload, *Refusal) {
	var p WeftBugReportPayload
	if err := decodePayloadStrict(data, &p); err != nil {
		return nil, refuse(ReasonMalformed, "payload is not a valid weft bug-report record: %v", err)
	}
	if strings.TrimSpace(p.ReportID) == "" {
		return nil, refuse(ReasonMalformed, "weft bug-report record has no report_id")
	}
	switch p.Action {
	case BugReportCreate:
		if strings.TrimSpace(p.Title) == "" {
			return nil, refuse(ReasonMalformed, "weft bug report has no title")
		}
		if p.BugID != "" {
			return nil, refuse(ReasonMalformed, "weft bug report unexpectedly names bug_id %q", p.BugID)
		}
	case BugReportNote:
		if strings.TrimSpace(p.BugID) == "" {
			return nil, refuse(ReasonMalformed, "weft bug note names no bug_id")
		}
		if strings.TrimSpace(p.Note) == "" {
			return nil, refuse(ReasonMalformed, "weft bug note has no note text")
		}
		if p.Title != "" || p.Kind != "" || p.Scope != "" || p.Likelihood != "" ||
			p.Severity != "" || p.Fingerprint != "" || p.JobID != nil || p.Host != "" ||
			p.Summary != "" || p.Detail != "" {
			return nil, refuse(ReasonMalformed, "weft bug note carries report-only fields")
		}
	default:
		return nil, refuse(ReasonMalformed, "weft bug-report record names unknown action %q", p.Action)
	}
	return &p, nil
}

// EncodeWeftBugReportPayload serializes a bug fact for signing.
func EncodeWeftBugReportPayload(p WeftBugReportPayload) ([]byte, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encode weft bug-report payload: %w", err)
	}
	return data, nil
}

func decodePayloadStrict(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("payload contains a second JSON value")
		}
		return err
	}
	return nil
}

// WeftJobPayload is the payload for KindWeftJobSubmission.
//
// The authority fields live here rather than in the shared envelope. They are
// no less protected for it: the envelope's PayloadDigest is inside the
// signature, so altering this payload invalidates the pointer that names it. A
// party who can write the inbound store but cannot sign still cannot raise a
// ceiling or redirect execution.
//
// What the move buys is that a caller with no notion of spend or placement
// never carries authority fields it must set to null, and the hub cannot read
// authority out of an envelope whose kind has none.
type WeftJobPayload struct {
	Command           string            `json:"command"`
	WorkingDir        string            `json:"working_dir,omitempty"`
	Project           string            `json:"project,omitempty"`
	Description       string            `json:"description,omitempty"`
	SourceDigest      string            `json:"source_digest,omitempty"`
	QueueParams       json.RawMessage   `json:"queue_params,omitempty"`
	SpendCeilingUSD   float64           `json:"spend_ceiling_usd"`
	TargetConstraints TargetConstraints `json:"target_constraints"`
}

// TargetConstraints is what the edge asks for. It is a request, not a grant:
// the hub checks it against its own allowlist, which no submission can extend.
type TargetConstraints struct {
	// Hosts names acceptable execution targets. Empty means the hub chooses
	// from its allowlist by its normal placement rules.
	Hosts []string `json:"hosts,omitempty"`
	// GPU is a weft GPU constraint expression, e.g. "nvidia>=24GB".
	GPU string `json:"gpu,omitempty"`
	// Tags are weft placement tags carried with the job.
	Tags []string `json:"tags,omitempty"`
}

// ParseWeftJobPayload decodes a verified payload.
//
// It takes the digest-checked bytes only. There is no overload that accepts
// unverified bytes, so a caller cannot parse a payload the hub has not
// confirmed matches the signed pointer.
func ParseWeftJobPayload(data []byte) (*WeftJobPayload, *Refusal) {
	var p WeftJobPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, refuse(ReasonMalformed, "payload is not a valid weft job submission: %v", err)
	}
	if p.Command == "" {
		return nil, refuse(ReasonMalformed, "weft job submission has no command")
	}
	return &p, nil
}

// EncodeWeftJobPayload serializes a job submission for signing.
func EncodeWeftJobPayload(p WeftJobPayload) ([]byte, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encode weft job payload: %w", err)
	}
	return data, nil
}

// PlanEndedPayload asks the hub to close a plan's spend authority.
//
// This is a downward-only request: honoring it can only reduce what the
// submitter may do. That is why it needs no trust beyond the ordinary
// signature, and why it does not weaken the rule that an edge must never
// assert its own liveness.
type PlanEndedPayload struct {
	PlanID string `json:"plan_id"`
	Reason string `json:"reason,omitempty"`
}

// ParsePlanEndedPayload decodes a verified termination request.
func ParsePlanEndedPayload(data []byte) (*PlanEndedPayload, *Refusal) {
	var p PlanEndedPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, refuse(ReasonMalformed, "payload is not a valid plan-ended request: %v", err)
	}
	if p.PlanID == "" {
		return nil, refuse(ReasonMalformed, "plan-ended request names no plan")
	}
	return &p, nil
}

// EncodePlanEndedPayload serializes a termination request for signing.
func EncodePlanEndedPayload(p PlanEndedPayload) ([]byte, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encode plan-ended payload: %w", err)
	}
	return data, nil
}

// ApplyPlanEnded closes the spend authority of the plan that signed the request.
//
// A request may only end its OWN plan. Without that check, anyone who can write
// the inbound bucket could close other plans' authority, turning a safety
// mechanism into a denial-of-service surface. Signed and self-scoped, the worst
// case is that whoever already holds a plan's key can end that plan — which is
// either the plan itself, or a host compromise with better options available.
// Returns a *Refusal for a terminal decision about the submission, and an
// error for a hub-side fault — a missing keyring or a failed write is our
// problem, not a verdict on the submitter's request, and the caller must retry
// rather than record a refusal.
func ApplyPlanEnded(ring *Keyring, v *VerifiedEnvelope, p *PlanEndedPayload, now time.Time) (*Refusal, error) {
	if ring == nil {
		return nil, fmt.Errorf(
			"no keyring is available, so plan %s's authority cannot be closed", v.PlanID())
	}
	if v.PlanID() == "" {
		return refuse(ReasonKeyNotBoundToPlan,
			"key %s is not bound to a plan, so it cannot end one", v.KeyID()), nil
	}
	if p.PlanID != v.PlanID() {
		return refuse(ReasonPlanScopeMismatch,
			"signature valid (key %s, plan %s); a termination request may only end its own plan, not %q",
			v.KeyID(), v.PlanID(), p.PlanID), nil
	}
	// EndAuthority persists, so the closed window survives a hub restart.
	if err := ring.EndAuthority(v.KeyID(), now); err != nil {
		return nil, fmt.Errorf("end authority for key %s: %w", v.KeyID(), err)
	}
	return nil, nil
}
