package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/edgeview"
	"github.com/osteele/weft/internal/ids"
	"github.com/spf13/cobra"
)

// jobDetailView is the job-scoped document published by the hub. Each command
// has a presentation model because status and info intentionally expose
// different text surfaces for the same ledger row.
type jobDetailView struct {
	JobID  string              `json:"job_id"`
	Status jobPresentationView `json:"status"`
	Info   jobPresentationView `json:"info"`
}

// jobPresentationView is a stable, serializable command model. Text includes
// its trailing newline, so rendering is a byte-preserving write rather than a
// second formatting implementation on the edge.
type jobPresentationView struct {
	Version     int                  `json:"version"`
	JobID       string               `json:"job_id"`
	Host        string               `json:"host"`
	Status      string               `json:"status"`
	Description string               `json:"description,omitempty"`
	Directory   string               `json:"directory,omitempty"`
	Command     string               `json:"command,omitempty"`
	Text        string               `json:"text"`
	Warnings    string               `json:"warnings,omitempty"`
	Source      *edgeview.Provenance `json:"source,omitempty"`
}

const jobPresentationVersion = 1

func buildJobDetailView(database *sql.DB, jobID int64, statusHints bool) (jobDetailView, error) {
	statusView, err := buildJobStatusPresentation(database, jobID, statusHints)
	if err != nil {
		return jobDetailView{}, err
	}
	infoView, err := buildJobInfoPresentation(database, jobID)
	if err != nil {
		return jobDetailView{}, err
	}
	return jobDetailView{JobID: statusView.JobID, Status: statusView, Info: infoView}, nil
}

func buildJobStatusPresentation(database *sql.DB, jobID int64, statusHints bool) (jobPresentationView, error) {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return jobPresentationView{}, fmt.Errorf("get job %s: %w", ids.FormatJobID(jobID), err)
	}
	if job == nil {
		return jobPresentationView{}, fmt.Errorf("job %s not found", ids.FormatJobID(jobID))
	}
	hydrateQueueBlockedReasons([]*db.Job{job})
	applyAttemptOutcomeOverrides(database, []*db.Job{job})

	formattedID := ids.FormatJobID(job.ID)
	var statusBuf bytes.Buffer
	provenance, err := loadAttemptDisplayProvenance(database, job)
	if err != nil {
		return jobPresentationView{}, fmt.Errorf("load attempt provenance for %s: %w", formattedID, err)
	}
	printJobStatusWithProvenance(&statusBuf, database, job, statusHints, provenance)
	return newJobPresentationView(job, formattedID, statusBuf.String()), nil
}

func buildJobInfoPresentation(database *sql.DB, jobID int64) (jobPresentationView, error) {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return jobPresentationView{}, fmt.Errorf("get job %s: %w", ids.FormatJobID(jobID), err)
	}
	if job == nil {
		return jobPresentationView{}, fmt.Errorf("job %s not found", ids.FormatJobID(jobID))
	}
	hydrateQueueBlockedReasons([]*db.Job{job})
	formattedID := ids.FormatJobID(job.ID)
	var infoBuf, infoErr bytes.Buffer
	if err := renderJobInfoFromLedger(&infoBuf, &infoErr, database, job); err != nil {
		return jobPresentationView{}, fmt.Errorf("build info for %s: %w", formattedID, err)
	}
	view := newJobPresentationView(job, formattedID, infoBuf.String())
	view.Warnings = infoErr.String()
	return view, nil
}

func newJobPresentationView(job *db.Job, formattedID, text string) jobPresentationView {
	return jobPresentationView{
		Version:     jobPresentationVersion,
		JobID:       formattedID,
		Host:        job.TargetDisplay(),
		Status:      job.EffectiveStatus(),
		Description: job.Description,
		Directory:   job.DisplayWorkingDir(),
		Command:     job.Command,
		Text:        text,
	}
}

func produceJobDetailSection(ctx context.Context, deps edgeViewDeps, jobID int64) ([]byte, error) {
	view, err := buildJobDetailView(deps.DB, jobID, true)
	if err != nil {
		return nil, err
	}
	return marshalViewJSON(view)
}

func renderJobPresentation(w, errW io.Writer, view jobPresentationView) error {
	if _, err := io.WriteString(w, view.Text); err != nil {
		return err
	}
	if view.Warnings != "" {
		if _, err := io.WriteString(errW, view.Warnings); err != nil {
			return err
		}
	}
	return nil
}

func runJobStatusEdge(cmd *cobra.Command, args []string, em *edgeMirrorRuntime) error {
	if len(args) == 0 {
		return fmt.Errorf("the hub view serves status for named jobs; use `weft list --active` for the mirrored active-job index")
	}
	if statusWait {
		return fmt.Errorf("--wait needs live ledger updates; run it on the hub")
	}
	if statusSync || statusFast || statusSSHTimeout > 0 {
		return fmt.Errorf("status sync flags need live host access; omit them to read the hub view")
	}
	jobIDs, err := ParseJobIDs(args)
	if err != nil {
		return err
	}
	views := make([]jobPresentationView, 0, len(jobIDs))
	for _, jobID := range jobIDs {
		view, prov, err := fetchJobDetailView(cmd, em, jobID)
		if err != nil {
			return err
		}
		view.Status.Source = &prov
		views = append(views, view.Status)
	}
	if statusJSON {
		return writeJobPresentationJSON(cmd.OutOrStdout(), views)
	}
	for i, view := range views {
		if i > 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "---")
		}
		if err := renderJobPresentation(cmd.OutOrStdout(), cmd.ErrOrStderr(), view); err != nil {
			return err
		}
		printEdgeProvenance(cmd.OutOrStdout(), *view.Source)
	}
	return nil
}

func runJobInfoEdge(cmd *cobra.Command, args []string, em *edgeMirrorRuntime) error {
	if jobInfoSync || jobInfoAllAttempts {
		return fmt.Errorf("info sync and expanded-attempt flags need the live ledger; omit them to read the hub view")
	}
	jobIDs, err := ParseJobIDs(args)
	if err != nil {
		return err
	}
	views := make([]jobPresentationView, 0, len(jobIDs))
	for _, jobID := range jobIDs {
		view, prov, err := fetchJobDetailView(cmd, em, jobID)
		if err != nil {
			return err
		}
		view.Info.Source = &prov
		views = append(views, view.Info)
	}
	if jobInfoJSON {
		return writeJobPresentationJSON(cmd.OutOrStdout(), views)
	}
	for i, view := range views {
		if i > 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "---")
		}
		if err := renderJobPresentation(cmd.OutOrStdout(), cmd.ErrOrStderr(), view); err != nil {
			return err
		}
		printEdgeProvenance(cmd.OutOrStdout(), *view.Source)
	}
	return nil
}

func fetchJobDetailView(cmd *cobra.Command, em *edgeMirrorRuntime, jobID int64) (jobDetailView, edgeview.Provenance, error) {
	section := edgeview.JobDetailSection(ids.FormatJobID(jobID))
	body, prov, err := em.fetch(cmd, section)
	if err != nil {
		return jobDetailView{}, edgeview.Provenance{}, err
	}
	var view jobDetailView
	if err := json.Unmarshal(body, &view); err != nil {
		return jobDetailView{}, edgeview.Provenance{}, fmt.Errorf("decode hub view section %s: %w", section, err)
	}
	want := ids.FormatJobID(jobID)
	if view.JobID != want {
		return jobDetailView{}, edgeview.Provenance{}, fmt.Errorf("hub view section %s contains job %s", section, view.JobID)
	}
	return view, prov, nil
}

func writeJobPresentationJSON(w io.Writer, views []jobPresentationView) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if len(views) == 1 {
		return enc.Encode(views[0])
	}
	return enc.Encode(views)
}
