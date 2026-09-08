package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/daemoncontrol"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/edge"
	"github.com/osteele/weft/internal/ops"
	"github.com/spf13/cobra"
)

type filesystemSourceWriter struct {
	transport edge.Transport
}

type failingAckTransport struct {
	edge.Transport
	failKey string
}

func (t *failingAckTransport) Put(ctx context.Context, key string, body []byte) error {
	if key == t.failKey {
		return errors.New("injected acknowledgement write failure")
	}
	return t.Transport.Put(ctx, key, body)
}

func writeAckForFirstPointer(ctx context.Context, transport edge.Transport, makeAck func(string) edge.Ack) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		keys, err := transport.List(ctx, edge.PrefixInbox)
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			nonce := strings.TrimPrefix(keys[0], edge.PrefixInbox)
			data, err := json.Marshal(makeAck(nonce))
			if err != nil {
				return err
			}
			return transport.Put(ctx, edge.AckKey(nonce), data)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func TestShouldPollEdgeInboxPinsRoleAndConfiguration(t *testing.T) {
	for name, testCase := range map[string]struct {
		cfg  *config.Config
		want bool
	}{
		"configured hub":  {cfg: &config.Config{Edge: config.EdgeConfig{Role: "hub", Inbound: config.R2Config{Bucket: "inbox"}}}, want: true},
		"default hub":     {cfg: &config.Config{}, want: false},
		"configured edge": {cfg: &config.Config{Edge: config.EdgeConfig{Role: "edge", Inbound: config.R2Config{Bucket: "inbox"}}}, want: false},
		"nil":             {cfg: nil, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := shouldPollEdgeInbox(testCase.cfg); got != testCase.want {
				t.Fatalf("shouldPollEdgeInbox = %t, want %t", got, testCase.want)
			}
		})
	}
}

func TestEdgeInboxStateRoundTripAndDoctor(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	want := edgeInboxState{
		LastAttempt:        time.Now().Add(-time.Minute),
		LastSuccessfulPoll: time.Now().Add(-30 * time.Second),
		PendingPointers:    3,
		PollError:          "inbound timeout",
	}
	if err := saveEdgeInboxState(want); err != nil {
		t.Fatalf("saveEdgeInboxState: %v", err)
	}
	got, err := loadEdgeInboxState()
	if err != nil {
		t.Fatalf("loadEdgeInboxState: %v", err)
	}
	if got.PendingPointers != 3 || got.PollError != want.PollError || !got.LastSuccessfulPoll.Equal(want.LastSuccessfulPoll) {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	printEdgeInboxDoctor(cmd, &config.Config{Edge: config.EdgeConfig{Inbound: config.R2Config{Bucket: "inbox"}}}, "hub")
	if !strings.Contains(out.String(), "3 pointer(s) pending") || !strings.Contains(out.String(), "Inbox error: inbound timeout") {
		t.Fatalf("doctor output = %q", out.String())
	}
}

func TestSubmitEdgeJobMissingAckIsUnknown(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	signer, _, err := edge.GenerateSigner("edge-alpha-plan-1", "edge-alpha")
	if err != nil {
		t.Fatal(err)
	}
	workingDir := t.TempDir()
	if err := os.WriteFile(workingDir+"/main.go", []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sourceMetadata, sourceDigest, sourceClosure, err := buildEdgeSourceClosure(workingDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	oneSecond := 1
	cfg := &config.Config{Edge: config.EdgeConfig{AdmissionWaitSeconds: &oneSecond}}
	var out bytes.Buffer
	err = submitEdgeJob(context.Background(), &out, &edge.Runtime{
		Role: "edge", Transport: transport, Signer: signer, SubmitterHost: "edge-alpha",
	}, cfg, ops.QueueJobParams{
		WorkingDir: workingDir, Command: "echo pending",
		Metadata: &db.JobMetadata{Source: sourceMetadata},
	}, sourceDigest, sourceClosure)
	if err != nil {
		t.Fatalf("submitEdgeJob: %v", err)
	}
	if !strings.Contains(out.String(), "the job id is assigned on admission") || !strings.Contains(out.String(), "weft edge wait ") {
		t.Fatalf("timeout output = %q", out.String())
	}
	keys, err := transport.List(context.Background(), edge.PrefixInbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("inbox pointers after missing ack = %v, want one standing submission", keys)
	}
}

func TestDeploymentSourceDigestIdentifiesExecutable(t *testing.T) {
	digest, err := deploymentSourceDigest()
	if err != nil {
		t.Fatal(err)
	}
	if len(digest) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("deployment digest = %q", digest)
	}
}

func TestSubmitEdgeJobPrintsHubJobID(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	signer, _, err := edge.GenerateSigner("edge-alpha-plan-1", "edge-alpha")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ackResult := make(chan error, 1)
	go func() {
		ackResult <- writeAckForFirstPointer(ctx, transport, func(nonce string) edge.Ack {
			return edge.Ack{Version: 1, Nonce: nonce, Accepted: true, JobID: 42, Phase: "admitted", AckedAt: time.Now()}
		})
	}()
	fiveSeconds := 5
	var out bytes.Buffer
	if err := submitEdgeJob(ctx, &out, &edge.Runtime{
		Role: "edge", Transport: transport, Signer: signer, SubmitterHost: "edge-alpha",
	}, &config.Config{Edge: config.EdgeConfig{AdmissionWaitSeconds: &fiveSeconds}},
		ops.QueueJobParams{WorkingDir: t.TempDir(), Command: "echo accepted"}, "", nil); err != nil {
		t.Fatalf("submitEdgeJob: %v", err)
	}
	if err := <-ackResult; err != nil {
		t.Fatalf("write acknowledgement: %v", err)
	}
	if out.String() != "wj42\n" {
		t.Fatalf("output = %q, want exactly job id", out.String())
	}
}

func TestSubmitEdgeJobPrintsRefusalCheckAndDetail(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	signer, _, err := edge.GenerateSigner("edge-alpha-plan-1", "edge-alpha")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ackResult := make(chan error, 1)
	go func() {
		ackResult <- writeAckForFirstPointer(ctx, transport, func(nonce string) edge.Ack {
			return edge.Ack{Version: 1, Nonce: nonce, ReasonCode: string(edge.ReasonTargetNotAllowed), Detail: "target detail verbatim", AckedAt: time.Now()}
		})
	}()
	fiveSeconds := 5
	var out bytes.Buffer
	err = submitEdgeJob(ctx, &out, &edge.Runtime{
		Role: "edge", Transport: transport, Signer: signer, SubmitterHost: "edge-alpha",
	}, &config.Config{Edge: config.EdgeConfig{AdmissionWaitSeconds: &fiveSeconds}},
		ops.QueueJobParams{WorkingDir: t.TempDir(), Command: "echo refused"}, "", nil)
	if err == nil {
		t.Fatal("submitEdgeJob accepted a refusal")
	}
	if ackErr := <-ackResult; ackErr != nil {
		t.Fatalf("write acknowledgement: %v", ackErr)
	}
	want := "Refused: target_not_allowed: target detail verbatim\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

// This kills the mutation that marks cancel as served at the gate but lets its
// RunE continue into db.Open on an edge.
func TestRunCancelEdgeBuildsControlPayloadWithoutLedger(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	signer, _, err := edge.GenerateSigner("edge-alpha-plan-1", "edge-alpha")
	if err != nil {
		t.Fatal(err)
	}
	previous := activeEdgeSubmit
	activeEdgeSubmit = &edge.Runtime{Role: "edge", Transport: transport, Signer: signer, SubmitterHost: "edge-alpha"}
	t.Cleanup(func() { activeEdgeSubmit = previous })
	resetLedgerRefusal := db.RefuseLocalLedger("edge cancel seam test")
	t.Cleanup(resetLedgerRefusal)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ackResult := make(chan error, 1)
	go func() {
		ackResult <- writeAckForFirstPointer(ctx, transport, func(nonce string) edge.Ack {
			return edge.Ack{Version: 1, Nonce: nonce, Accepted: true, Detail: "Job wj42: cancel completed", AckedAt: time.Now()}
		})
	}()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runCancelWithParser(cmd, []string{"wj42"}, ParseJobIDs); err != nil {
		t.Fatalf("runCancelWithParser: %v", err)
	}
	if err := <-ackResult; err != nil {
		t.Fatal(err)
	}
	if out.String() != "Job wj42: cancel completed\n" {
		t.Fatalf("output = %q", out.String())
	}
	payloadKeys, err := transport.List(ctx, edge.PrefixPayload)
	if err != nil || len(payloadKeys) != 1 {
		t.Fatalf("payload keys=%v err=%v", payloadKeys, err)
	}
	data, err := transport.Get(ctx, payloadKeys[0])
	if err != nil {
		t.Fatal(err)
	}
	payload, refusal := edge.ParseWeftJobControlPayload(data)
	if refusal != nil || payload.JobID != 42 || payload.Action != edge.ControlCancel || payload.RequestID == "" {
		t.Fatalf("payload=%#v refusal=%v", payload, refusal)
	}
}

// This kills the mutation that signs an edit's --max-spend flag without the
// matching numeric authority field checked during admission.
func TestEdgeControlSpendCeilingMatchesEditFlag(t *testing.T) {
	got, err := edgeControlSpendCeilingUSD(edge.ControlEdit, map[string]string{"max-spend": "$8.30"})
	if err != nil || got != 8.30 {
		t.Fatalf("edit spend ceiling = %.2f, err=%v; want 8.30", got, err)
	}
	got, err = edgeControlSpendCeilingUSD(edge.ControlCancel, map[string]string{"max-spend": "$99"})
	if err != nil || got != 0 {
		t.Fatalf("cancel spend ceiling = %.2f, err=%v; want 0", got, err)
	}
}

func TestRunRunEdgeBuildsAndSubmitsWithoutLedger(t *testing.T) {
	resetRunGlobals(t)
	workingDir := t.TempDir()
	if err := os.WriteFile(workingDir+"/main.go", []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runDir = workingDir
	runProject = "edge-project"
	runTags = []string{"benchmark"}
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	signer, _, err := edge.GenerateSigner("edge-alpha-plan-1", "edge-alpha")
	if err != nil {
		t.Fatal(err)
	}
	activeEdgeSubmit = &edge.Runtime{Role: "edge", Transport: transport, Signer: signer, SubmitterHost: "edge-alpha"}
	resetLedgerRefusal := db.RefuseLocalLedger("edge run seam test")
	defer resetLedgerRefusal()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ackResult := make(chan error, 1)
	go func() {
		ackResult <- writeAckForFirstPointer(ctx, transport, func(nonce string) edge.Ack {
			return edge.Ack{Version: 1, Nonce: nonce, Accepted: true, JobID: 42, AckedAt: time.Now()}
		})
	}()
	cmd := newRunTestCommand()
	cmd.SetContext(ctx)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := runRun(cmd, []string{"echo edge-seam"}); err != nil {
		t.Fatalf("runRun: %v\n%s", err, out.String())
	}
	if err := <-ackResult; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "wj42\n") {
		t.Fatalf("run output = %q", out.String())
	}
	payloadKeys, err := transport.List(ctx, edge.PrefixPayload)
	if err != nil {
		t.Fatal(err)
	}
	if len(payloadKeys) != 1 {
		t.Fatalf("payload keys = %v, want one", payloadKeys)
	}
	payloadBytes, err := transport.Get(ctx, payloadKeys[0])
	if err != nil {
		t.Fatal(err)
	}
	payload, refusal := edge.ParseWeftJobPayload(payloadBytes)
	if refusal != nil {
		t.Fatalf("ParseWeftJobPayload: %v", refusal)
	}
	var request ops.QueueJobParams
	if err := json.Unmarshal(payload.QueueParams, &request); err != nil {
		t.Fatal(err)
	}
	if request.Command != "echo edge-seam" || request.Project != "edge-project" || len(request.Tags) != 1 || request.Tags[0] != "benchmark" {
		t.Fatalf("submitted request = %#v", request)
	}
	if payload.SourceDigest == "" {
		t.Fatal("submitted payload has no source closure digest")
	}
}

func TestRefusalAckPreservesCheckAndDetail(t *testing.T) {
	now := time.Now().UTC()
	for name, testCase := range map[string]struct {
		refusal *edge.Refusal
		phase   string
	}{
		"authentication": {refusal: &edge.Refusal{Code: edge.ReasonBadSignature, Detail: "signature detail verbatim"}, phase: "authentication"},
		"authorization":  {refusal: &edge.Refusal{Code: edge.ReasonTargetNotAllowed, Detail: "target detail verbatim"}, phase: "authorization"},
	} {
		t.Run(name, func(t *testing.T) {
			ack := refusalAck("nonce", "hub-alpha", testCase.refusal, now)
			if ack.Accepted || ack.ReasonCode != string(testCase.refusal.Code) || ack.Detail != testCase.refusal.Detail || ack.Phase != testCase.phase {
				t.Fatalf("ack = %#v", ack)
			}
		})
	}
}

func TestDecodeEdgeRunRequestRejectsUnknownFields(t *testing.T) {
	if _, err := decodeEdgeRunRequest([]byte(`{"Command":"echo hi","FutureAuthority":true}`)); err == nil {
		t.Fatal("decodeEdgeRunRequest accepted an unknown field")
	}
}

// This kills the mutation that visits merged command flags and signs root
// process flags such as --verbose into a job-control payload.
func TestChangedCommandFlagsExcludesRootPersistentFlags(t *testing.T) {
	root := &cobra.Command{Use: "weft"}
	root.PersistentFlags().Bool("verbose", false, "")
	root.PersistentFlags().Bool("allow-stale", false, "")
	root.PersistentFlags().String("edge-view-transport", "", "")
	child := &cobra.Command{Use: "edit"}
	child.Flags().Bool("retry", false, "")
	root.AddCommand(child)
	for name, value := range map[string]string{
		"verbose": "true", "allow-stale": "true", "edge-view-transport": "fs",
	} {
		if err := root.PersistentFlags().Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := child.Flags().Set("retry", "true"); err != nil {
		t.Fatal(err)
	}
	if err := child.ParseFlags(nil); err != nil {
		t.Fatal(err)
	}
	flags := changedCommandFlags(child)
	if len(flags) != 1 || flags["retry"] != "true" {
		t.Fatalf("changed command flags = %#v, want only retry=true", flags)
	}
}

func (w filesystemSourceWriter) PutObject(ctx context.Context, key string, body io.Reader, _ string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	return w.transport.Put(ctx, key, data)
}

// This kills mutations that keep an admitted job nonce recorded without a row,
// pin a hub-chosen request to the first target, omit its persisted allowlist,
// or acknowledge an unplaced job as placed.
func TestEdgeSubmissionEndToEndFilesystem(t *testing.T) {
	ctx := context.Background()
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSTransport: %v", err)
	}
	database := db.SetupTestDB(t)

	now := time.Now().UTC()
	signer, publicKey, err := edge.GenerateSigner("edge-alpha-plan-1", "edge-alpha")
	if err != nil {
		t.Fatalf("GenerateSigner: %v", err)
	}
	publicKey.PlanID = "plan-1"
	publicKey.NotBefore = now.Add(-time.Hour)
	publicKey.NotAfter = now.Add(time.Hour)
	publicKey.SpendCeilingUSD = 10
	keyring := edge.NewKeyring()
	if err := keyring.Add(publicKey); err != nil {
		t.Fatalf("Keyring.Add: %v", err)
	}
	policy, err := edge.NewPolicy("hub-alpha", []string{"host-alpha"}, 10)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	seen, err := edge.NewFileSeenSet(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileSeenSet: %v", err)
	}
	hub := &edge.Runtime{
		Transport: transport,
		Keyring:   keyring,
		Policy:    policy,
		Seen:      seen,
		Kinds:     edge.DefaultKindRegistry(),
	}
	edgeRuntime := &edge.Runtime{Role: "edge", Transport: transport, Signer: signer, SubmitterHost: "edge-alpha"}

	workingDir := t.TempDir()
	if err := os.WriteFile(workingDir+"/main.go", []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write source fixture: %v", err)
	}
	sourceMetadata, sourceDigest, sourceClosure, err := buildEdgeSourceClosure(workingDir, nil)
	if err != nil {
		t.Fatalf("buildEdgeSourceClosure: %v", err)
	}
	sourceKey := sourceMetadata.Pin.Roots[0].R2Key
	request := ops.QueueJobParams{
		WorkingDir:   workingDir,
		Command:      "echo from-edge",
		Project:      "edge-project",
		Metadata:     &db.JobMetadata{Source: sourceMetadata},
		CLIOverrides: &db.CLIResourceOverrides{},
	}
	queueParams, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal queue params: %v", err)
	}
	payload, err := edge.EncodeWeftJobPayload(edge.WeftJobPayload{
		Command: "echo from-edge", WorkingDir: request.WorkingDir, Project: request.Project,
		SourceDigest: sourceDigest, QueueParams: queueParams,
		SpendCeilingUSD: 5,
	})
	if err != nil {
		t.Fatalf("EncodeWeftJobPayload: %v", err)
	}
	result, err := edge.Submit(ctx, edgeRuntime.Transport, edgeRuntime.Signer, edge.SubmitRequest{
		SubmitterHost: edgeRuntime.SubmitterHost, DeploymentSourceDigest: "sha256:deployment",
		Kind: edge.KindWeftJobSubmission, Payload: payload,
		SourceClosure: sourceClosure, SourceDigest: sourceDigest,
	}, now)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	pointerObject, err := transport.Get(ctx, edge.InboxKey(result.Nonce))
	if err != nil {
		t.Fatalf("read submitted pointer: %v", err)
	}
	admission, refusal, err := edge.Admit(ctx, transport, pointerObject, edge.AdmitOptions{
		Verify: edgeVerifyOptions(hub), Policy: hub.Policy, Seen: hub.Seen,
	})
	if err != nil || refusal != nil || admission == nil {
		t.Fatalf("pre-admit recovery fixture: admission=%#v refusal=%#v err=%v", admission, refusal, err)
	}
	if pending, err := pollEdgeInboxOnce(ctx, database, edgeInboxDeps{
		Transport: transport, Runtime: hub,
		SourceStore: filesystemSourceWriter{transport: transport}, HubHost: "hub-alpha",
	}); err == nil || pending != 1 {
		t.Fatalf("first recovery poll: pending=%d err=%v, want pending=1 with rollback error", pending, err)
	}
	if already, err := seen.Seen(result.Nonce); err != nil || already {
		t.Fatalf("rolled-back nonce seen=%t err=%v", already, err)
	}

	pending, err := pollEdgeInboxOnce(ctx, database, edgeInboxDeps{
		Transport: transport, Runtime: hub,
		SourceStore: filesystemSourceWriter{transport: transport}, HubHost: "hub-alpha",
	})
	if err != nil {
		t.Fatalf("pollEdgeInboxOnce: %v", err)
	}
	if pending != 0 {
		t.Fatalf("pending pointers after poll = %d, want 0", pending)
	}
	jobID, found, err := db.FindJobIDByEdgeNonce(database, result.Nonce)
	if err != nil {
		t.Fatalf("FindJobIDByEdgeNonce: %v", err)
	}
	if !found {
		t.Fatal("signed edge submission did not create a job row")
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if got := strings.Join(job.EdgeAuthorizedTargets, ","); got != "host-alpha" {
		t.Fatalf("ordinary job read authorized targets = %q, want host-alpha", got)
	}
	jobList, err := db.ListJobs(database, "", "", 0, nil, "all")
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	var listedJob *db.Job
	for _, listed := range jobList {
		if listed.ID == jobID {
			listedJob = listed
			break
		}
	}
	if listedJob == nil {
		t.Fatalf("admitted job %d is absent from the hub job list", jobID)
	}
	if got := strings.Join(listedJob.EdgeAuthorizedTargets, ","); got != "host-alpha" {
		t.Fatalf("listed job authorized targets = %q, want host-alpha", got)
	}
	if err := db.PopulateEdgeSubmissionProvenance(database, []*db.Job{job}); err != nil {
		t.Fatalf("PopulateEdgeSubmissionProvenance: %v", err)
	}
	if job.EdgeSubmitterHost != "edge-alpha" {
		t.Errorf("submitter host = %q, want edge-alpha", job.EdgeSubmitterHost)
	}
	if job.EdgeSigningKeyID != "edge-alpha-plan-1" {
		t.Errorf("signing key = %q, want edge-alpha-plan-1", job.EdgeSigningKeyID)
	}
	if job.EdgeDeploymentDigest != "sha256:deployment" {
		t.Errorf("deployment digest = %q, want sha256:deployment", job.EdgeDeploymentDigest)
	}
	if job.Host != "" {
		t.Errorf("placement host = %q, want empty for hub-chosen placement", job.Host)
	}
	var infoOut, infoErr bytes.Buffer
	infoJob := *job
	infoJob.Host = ""
	if err := renderJobInfoFromLedger(&infoOut, &infoErr, database, &infoJob); err != nil {
		t.Fatalf("renderJobInfoFromLedger: %v", err)
	}
	if line := "Submitted from: edge-alpha (key edge-alpha-plan-1, deployment sha256:deployment, nonce " + result.Nonce + ")"; !strings.Contains(infoOut.String(), line) {
		t.Errorf("job info missing provenance line %q:\n%s", line, infoOut.String())
	}
	storedSource, err := transport.Get(ctx, sourceKey)
	if err != nil {
		t.Fatalf("read installed source closure: %v", err)
	}
	if string(storedSource) != string(sourceClosure) {
		t.Errorf("installed source closure = %q, want %q", storedSource, sourceClosure)
	}
	ack, err := edgeFetchAck(ctx, hub, result.Nonce)
	if err != nil {
		t.Fatalf("edgeFetchAck: %v", err)
	}
	if !ack.Accepted || ack.JobID != jobID || ack.Phase != "placement_pending" {
		t.Errorf("ack = %#v, want accepted placement-pending job %d", ack, jobID)
	}
	if err := transport.Put(ctx, edge.InboxKey(result.Nonce), pointerObject); err != nil {
		t.Fatalf("restore committed pointer: %v", err)
	}
	if _, err := pollEdgeInboxOnce(ctx, database, edgeInboxDeps{
		Transport: transport, Runtime: hub,
		SourceStore: filesystemSourceWriter{transport: transport}, HubHost: "hub-alpha",
	}); err != nil {
		t.Fatalf("repeat pollEdgeInboxOnce: %v", err)
	}
	var admittedRows int
	if err := database.QueryRow(`SELECT COUNT(*) FROM jobs WHERE edge_submission_nonce = ?`, result.Nonce).Scan(&admittedRows); err != nil {
		t.Fatalf("count admitted rows: %v", err)
	}
	if admittedRows != 1 {
		t.Errorf("admitted rows after replay = %d, want 1", admittedRows)
	}
	resolvedIDs, err := ParseJobIDs([]string{result.Nonce})
	if err != nil {
		t.Fatalf("ParseJobIDs(nonce): %v", err)
	}
	if len(resolvedIDs) != 1 || resolvedIDs[0] != jobID {
		t.Errorf("ParseJobIDs(nonce) = %v, want [%d]", resolvedIDs, jobID)
	}
	blocked, err := edge.Submit(ctx, edgeRuntime.Transport, edgeRuntime.Signer, edge.SubmitRequest{
		SubmitterHost: edgeRuntime.SubmitterHost, DeploymentSourceDigest: "sha256:deployment",
		Kind: edge.KindWeftJobSubmission, Payload: payload,
		SourceClosure: sourceClosure, SourceDigest: sourceDigest,
	}, now.Add(time.Millisecond))
	if err != nil {
		t.Fatalf("submit database-failure fixture: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close database fixture: %v", err)
	}
	if _, err := pollEdgeInboxOnce(ctx, database, edgeInboxDeps{
		Transport: transport, Runtime: hub,
		SourceStore: filesystemSourceWriter{transport: transport}, HubHost: "hub-alpha",
	}); err == nil {
		t.Fatal("poll with closed database succeeded")
	}
	seenAfterFailure, err := seen.Seen(blocked.Nonce)
	if err != nil {
		t.Fatalf("Seen after database failure: %v", err)
	}
	if seenAfterFailure {
		t.Fatal("database failure left the nonce recorded as admitted")
	}
	if _, err := transport.Get(ctx, edge.InboxKey(blocked.Nonce)); err != nil {
		t.Fatalf("database failure consumed the standing pointer: %v", err)
	}
}

func edgeControlFixture(t *testing.T, transport edge.Transport) (*edge.Runtime, map[string]*edge.Signer) {
	t.Helper()
	now := time.Now().UTC()
	keyring := edge.NewKeyring()
	signers := map[string]*edge.Signer{}
	for _, plan := range []string{"plan-owner", "plan-foreign"} {
		keyID := "edge-alpha-" + plan
		signer, publicKey, err := edge.GenerateSigner(keyID, "edge-alpha")
		if err != nil {
			t.Fatal(err)
		}
		publicKey.PlanID = plan
		publicKey.NotBefore = now.Add(-time.Hour)
		publicKey.NotAfter = now.Add(time.Hour)
		publicKey.SpendCeilingUSD = 10
		if err := keyring.Add(publicKey); err != nil {
			t.Fatal(err)
		}
		signers[plan] = signer
	}
	policy, err := edge.NewPolicy("hub-alpha", []string{"host-alpha"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	seen, err := edge.NewFileSeenSet(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &edge.Runtime{Transport: transport, Keyring: keyring, Policy: policy, Seen: seen, Kinds: edge.DefaultKindRegistry()}, signers
}

func recordEdgeOwnedJob(t *testing.T, database *sql.DB, signingKeyID string) int64 {
	t.Helper()
	nonce, err := edge.NewNonce(time.Now())
	if err != nil {
		t.Fatalf("create edge provenance nonce: %v", err)
	}
	jobID, err := ops.RecordQueuedJobContext(context.Background(), database, ops.QueueJobParams{
		WorkingDir: "/tmp/edge-control", Command: "echo controlled",
		EdgeProvenance: &db.EdgeSubmissionProvenance{
			SubmitterHost: "edge-alpha", SigningKeyID: signingKeyID,
			DeploymentSourceDigest: "sha256:deployment", Nonce: nonce,
		},
	})
	if err != nil {
		t.Fatalf("record edge-owned job: %v", err)
	}
	return jobID
}

func markEdgeControlJobRunning(t *testing.T, database *sql.DB, jobID int64) {
	t.Helper()
	launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := db.CreateAttempt(database, jobID, "", &launchID, db.StatusRunning)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET start_time = ? WHERE id = ?`, time.Now().Unix(), attemptID); err != nil {
		t.Fatal(err)
	}
}

func submitControlFixture(t *testing.T, transport edge.Transport, signer *edge.Signer, jobID int64, requestID string, action edge.JobControlAction, flags map[string]string) *edge.SubmitResult {
	return submitControlFixtureAt(t, transport, signer, jobID, requestID, action, flags, time.Now().UTC())
}

func submitControlFixtureAt(t *testing.T, transport edge.Transport, signer *edge.Signer, jobID int64, requestID string, action edge.JobControlAction, flags map[string]string, submittedAt time.Time) *edge.SubmitResult {
	t.Helper()
	spendCeilingUSD, err := edgeControlSpendCeilingUSD(action, flags)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := edge.EncodeWeftJobControlPayload(edge.WeftJobControlPayload{
		RequestID: requestID, JobID: jobID, Action: action, Flags: flags, SpendCeilingUSD: spendCeilingUSD,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := edge.Submit(context.Background(), transport, signer, edge.SubmitRequest{
		SubmitterHost: "edge-alpha", DeploymentSourceDigest: "sha256:control",
		Kind: edge.KindWeftJobControl, Payload: payload,
	}, submittedAt)
	if err != nil {
		t.Fatalf("submit control: %v", err)
	}
	return result
}

// This kills the mutation that returns from the poll loop after one pointer
// fails, which would leave every lexically later pointer unprocessed.
func TestEdgeInboxAckFailureDoesNotStarveLaterPointer(t *testing.T) {
	base, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	transport := &failingAckTransport{Transport: base}
	database := db.SetupTestDB(t)
	hub, signers := edgeControlFixture(t, transport)
	firstJob := recordEdgeOwnedJob(t, database, "edge-alpha-plan-owner")
	secondJob := recordEdgeOwnedJob(t, database, "edge-alpha-plan-owner")
	now := time.Now().UTC()
	first := submitControlFixtureAt(t, transport, signers["plan-owner"], firstJob, "first-cancel", edge.ControlCancel, nil, now)
	transport.failKey = edge.AckKey(first.Nonce)

	if pending, err := pollEdgeInboxOnce(context.Background(), database, edgeInboxDeps{
		Transport: transport, Runtime: hub, HubHost: "hub-alpha",
	}); err == nil || pending != 1 {
		t.Fatalf("first poll: pending=%d err=%v, want one pending acknowledgement failure", pending, err)
	}
	second := submitControlFixtureAt(t, transport, signers["plan-owner"], secondJob, "second-cancel", edge.ControlCancel, nil, now.Add(time.Second))
	if first.Nonce >= second.Nonce {
		t.Fatalf("fixture order = %s then %s, want lexical order", first.Nonce, second.Nonce)
	}

	pending, err := pollEdgeInboxOnce(context.Background(), database, edgeInboxDeps{
		Transport: transport, Runtime: hub, HubHost: "hub-alpha",
	})
	if err == nil || pending != 1 {
		t.Fatalf("second poll: pending=%d err=%v, want only failed first pointer pending", pending, err)
	}
	ack, err := edgeFetchAck(context.Background(), hub, second.Nonce)
	if err != nil || !ack.Accepted {
		t.Fatalf("second acknowledgement=%#v err=%v", ack, err)
	}
	if _, err := transport.Get(context.Background(), edge.InboxKey(second.Nonce)); !errors.Is(err, edge.ErrNotFound) {
		t.Fatalf("second pointer lookup error = %v, want confirmed absent", err)
	}
}

// This kills the mutation that requires a job-submission row while recovering
// a previously admitted control payload, which can never create such a row.
func TestEdgeInboxRecoversSeenControlWithoutJobSubmissionRow(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	database := db.SetupTestDB(t)
	hub, signers := edgeControlFixture(t, transport)
	jobID := recordEdgeOwnedJob(t, database, "edge-alpha-plan-owner")
	result := submitControlFixture(t, transport, signers["plan-owner"], jobID, "pre-admitted-cancel", edge.ControlCancel, nil)
	object, err := transport.Get(context.Background(), edge.InboxKey(result.Nonce))
	if err != nil {
		t.Fatal(err)
	}
	admission, refusal, err := edge.Admit(context.Background(), transport, object, edge.AdmitOptions{
		Verify: edgeVerifyOptions(hub), Policy: hub.Policy, Seen: hub.Seen,
		ControlJobs: func(jobID int64) (string, bool, error) {
			return db.FindJobEdgeSigningKey(database, jobID)
		},
	})
	if err != nil || refusal != nil || admission == nil {
		t.Fatalf("pre-admit control: admission=%#v refusal=%#v err=%v", admission, refusal, err)
	}

	if pending, err := pollEdgeInboxOnce(context.Background(), database, edgeInboxDeps{
		Transport: transport, Runtime: hub, HubHost: "hub-alpha",
	}); err != nil || pending != 0 {
		t.Fatalf("recovery poll: pending=%d err=%v", pending, err)
	}
	ack, err := edgeFetchAck(context.Background(), hub, result.Nonce)
	if err != nil || !ack.Accepted || !strings.Contains(ack.Detail, "original action detail is unavailable") {
		t.Fatalf("recovery acknowledgement=%#v err=%v", ack, err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil || job.EffectiveStatus() != db.StatusQueued {
		t.Fatalf("recovered control reran action: job=%#v err=%v", job, err)
	}
}

// This kills the mutation that converts an unknown jobs-database read failure
// into a terminal job-control acknowledgement.
func TestEdgeControlDatabaseFailureRemainsPending(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	database := db.SetupTestDB(t)
	hub, signers := edgeControlFixture(t, transport)
	jobID := recordEdgeOwnedJob(t, database, "edge-alpha-plan-owner")
	result := submitControlFixture(t, transport, signers["plan-owner"], jobID, "unknown-cancel", edge.ControlCancel, nil)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := pollEdgeInboxOnce(context.Background(), database, edgeInboxDeps{
		Transport: transport, Runtime: hub, HubHost: "hub-alpha",
	}); err == nil {
		t.Fatal("poll with closed jobs database succeeded")
	}
	if seen, err := hub.Seen.Seen(result.Nonce); err != nil || seen {
		t.Fatalf("nonce seen=%t err=%v, want unrecorded unknown result", seen, err)
	}
	if _, err := transport.Get(context.Background(), edge.InboxKey(result.Nonce)); err != nil {
		t.Fatalf("unknown control lost its pointer: %v", err)
	}
}

// This kills the mutation that treats a valid signature as job authority.
// A different plan's key signs correctly but must still be refused by name.
func TestEdgeControlForeignJobRefusedByAuthorityCheck(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	database := db.SetupTestDB(t)
	hub, signers := edgeControlFixture(t, transport)
	jobID := recordEdgeOwnedJob(t, database, "edge-alpha-plan-owner")
	result := submitControlFixture(t, transport, signers["plan-foreign"], jobID, "foreign-control", edge.ControlCancel, nil)

	if _, err := pollEdgeInboxOnce(context.Background(), database, edgeInboxDeps{
		Transport: transport, Runtime: hub, HubHost: "hub-alpha",
	}); err != nil {
		t.Fatalf("pollEdgeInboxOnce: %v", err)
	}
	ack, err := edgeFetchAck(context.Background(), hub, result.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Accepted || ack.ReasonCode != string(edge.ReasonJobControlAuthority) || !strings.Contains(ack.Detail, "job-control authority check failed") {
		t.Fatalf("foreign control ack = %#v", ack)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.EffectiveStatus() != db.StatusQueued {
		t.Fatalf("foreign control changed status to %q", job.EffectiveStatus())
	}
}

// This kills the mutation that authorizes ownership but never dispatches the
// admitted payload to the ordinary control handler.
func TestEdgeControlOwnedJobIsPerformed(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	database := db.SetupTestDB(t)
	hub, signers := edgeControlFixture(t, transport)
	jobID := recordEdgeOwnedJob(t, database, "edge-alpha-plan-owner")
	result := submitControlFixture(t, transport, signers["plan-owner"], jobID, "owned-control", edge.ControlCancel, nil)

	if _, err := pollEdgeInboxOnce(context.Background(), database, edgeInboxDeps{
		Transport: transport, Runtime: hub, HubHost: "hub-alpha",
	}); err != nil {
		t.Fatalf("pollEdgeInboxOnce: %v", err)
	}
	ack, err := edgeFetchAck(context.Background(), hub, result.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if !ack.Accepted || !strings.Contains(ack.Detail, "cancel completed") {
		t.Fatalf("owned control ack = %#v", ack)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.EffectiveStatus() != db.StatusCanceled {
		t.Fatalf("owned control status = %q, want canceled", job.EffectiveStatus())
	}
}

// This kills mutations that drop authenticated edit flags or fail to constrain
// an edge edit whose job has no stored spend ceiling.
func TestEdgeControlEditReplaysAuthenticatedFlags(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	database := db.SetupTestDB(t)
	hub, signers := edgeControlFixture(t, transport)
	jobID := recordEdgeOwnedJob(t, database, "edge-alpha-plan-owner")
	result := submitControlFixture(t, transport, signers["plan-owner"], jobID, "edit-control",
		edge.ControlEdit, map[string]string{"message": "edited at the hub"})

	if _, err := pollEdgeInboxOnce(context.Background(), database, edgeInboxDeps{
		Transport: transport, Runtime: hub, HubHost: "hub-alpha",
	}); err != nil {
		t.Fatalf("pollEdgeInboxOnce: %v", err)
	}
	ack, err := edgeFetchAck(context.Background(), hub, result.Nonce)
	if err != nil || !ack.Accepted {
		t.Fatalf("edit ack=%#v err=%v", ack, err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil || job.Description != "edited at the hub" {
		t.Fatalf("edited job=%#v err=%v", job, err)
	}
	if job.CLIResourceOverrides == nil || job.CLIResourceOverrides.MaxSpendCents == nil || *job.CLIResourceOverrides.MaxSpendCents != 1000 {
		t.Fatalf("edited job max-spend = %#v, want admitted 1000 cents", job.CLIResourceOverrides)
	}
}

// This kills either mutation that lets runEdit clear MaxSpendCents from
// --max-spend=0 or omits the admitted plan ceiling before retrying work.
func TestEdgeControlClearSpendInstallsPlanGrant(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	database := db.SetupTestDB(t)
	hub, signers := edgeControlFixture(t, transport)
	jobID := recordEdgeOwnedJob(t, database, "edge-alpha-plan-owner")
	retainedCents := 2000
	if err := db.SetJobCLIResourceOverrides(database, jobID, &db.CLIResourceOverrides{MaxSpendCents: &retainedCents}); err != nil {
		t.Fatal(err)
	}
	result := submitControlFixture(t, transport, signers["plan-owner"], jobID, "edit-retry-control",
		edge.ControlEdit, map[string]string{"max-spend": "0", "retry": "true"})

	if _, err := pollEdgeInboxOnce(context.Background(), database, edgeInboxDeps{
		Transport: transport, Runtime: hub, HubHost: "hub-alpha",
	}); err != nil {
		t.Fatalf("pollEdgeInboxOnce: %v", err)
	}
	ack, err := edgeFetchAck(context.Background(), hub, result.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if !ack.Accepted {
		t.Fatalf("edit retry ack = %#v", ack)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.CLIResourceOverrides == nil || job.CLIResourceOverrides.MaxSpendCents == nil || *job.CLIResourceOverrides.MaxSpendCents != 1000 {
		t.Fatalf("edit retry max-spend = %#v, want admitted 1000 cents", job.CLIResourceOverrides)
	}
}

// An edit that does not requeue work installs the grant too. A requeueing edit
// is covered by a second install on the requeue path, so only an edit that
// starts no work exercises the install in runEditWithSpendCeiling on its own.
// Without it `--max-spend 0` reaches the job as "cleared", which enforcement
// reads as unlimited.
func TestEdgeControlClearSpendWithoutRetryInstallsPlanGrant(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	database := db.SetupTestDB(t)
	hub, signers := edgeControlFixture(t, transport)
	jobID := recordEdgeOwnedJob(t, database, "edge-alpha-plan-owner")
	retainedCents := 2000
	if err := db.SetJobCLIResourceOverrides(database, jobID, &db.CLIResourceOverrides{MaxSpendCents: &retainedCents}); err != nil {
		t.Fatal(err)
	}
	result := submitControlFixture(t, transport, signers["plan-owner"], jobID, "edit-clear-no-retry",
		edge.ControlEdit, map[string]string{"max-spend": "0"})

	var pollErr error
	output := captureStdout(t, func() {
		_, pollErr = pollEdgeInboxOnce(context.Background(), database, edgeInboxDeps{
			Transport: transport, Runtime: hub, HubHost: "hub-alpha",
		})
	})
	if pollErr != nil {
		t.Fatalf("pollEdgeInboxOnce: %v", pollErr)
	}
	ack, err := edgeFetchAck(context.Background(), hub, result.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if !ack.Accepted {
		t.Fatalf("edit ack = %#v", ack)
	}
	// What the operator is told must match what was stored. Reporting "cleared"
	// for a ceiling that was set to the grant is a misreport about spending.
	if strings.Contains(output, "max-spend: cleared") {
		t.Fatalf("edit reported the ceiling as cleared while storing the grant:\n%s", output)
	}
	if !strings.Contains(output, "max-spend: $10.00") {
		t.Fatalf("edit did not report the installed $10.00 ceiling:\n%s", output)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.CLIResourceOverrides == nil || job.CLIResourceOverrides.MaxSpendCents == nil {
		t.Fatalf("edit --max-spend 0 cleared the ceiling; enforcement reads an absent ceiling as unlimited: %#v", job.CLIResourceOverrides)
	}
	if *job.CLIResourceOverrides.MaxSpendCents != 1000 {
		t.Fatalf("edit max-spend = %d cents, want admitted 1000", *job.CLIResourceOverrides.MaxSpendCents)
	}
}

// This kills the mutation that routes an admitted restart through the shared
// command without constraining a job that has no stored spend ceiling.
func TestEdgeControlRestartInstallsPlanGrant(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	database := db.SetupTestDB(t)
	hub, signers := edgeControlFixture(t, transport)
	jobID := recordEdgeOwnedJob(t, database, "edge-alpha-plan-owner")
	previousStatus := daemonStatusFunc
	daemonStatusFunc = func(daemoncontrol.Paths) (daemoncontrol.Status, error) {
		return daemoncontrol.Status{Live: true}, nil
	}
	t.Cleanup(func() { daemonStatusFunc = previousStatus })
	result := submitControlFixture(t, transport, signers["plan-owner"], jobID, "restart-control", edge.ControlRestart, nil)

	if _, err := pollEdgeInboxOnce(context.Background(), database, edgeInboxDeps{
		Transport: transport, Runtime: hub, HubHost: "hub-alpha",
	}); err != nil {
		t.Fatalf("pollEdgeInboxOnce: %v", err)
	}
	ack, err := edgeFetchAck(context.Background(), hub, result.Nonce)
	if err != nil || !ack.Accepted {
		t.Fatalf("restart acknowledgement=%#v err=%v", ack, err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.CLIResourceOverrides == nil || job.CLIResourceOverrides.MaxSpendCents == nil || *job.CLIResourceOverrides.MaxSpendCents != 1000 {
		t.Fatalf("restarted job max-spend = %#v, want admitted 1000 cents", job.CLIResourceOverrides)
	}
}

// This establishes at the full edge-inbox control level that a restart naming
// no money cannot raise an existing cap below the admitted plan grant. It kills
// an unconditional assignment of the admitted grant in the restart path.
func TestEdgeControlRestartPreservesLowerSpendCeiling(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	database := db.SetupTestDB(t)
	hub, signers := edgeControlFixture(t, transport)
	jobID := recordEdgeOwnedJob(t, database, "edge-alpha-plan-owner")
	lowerCents := 500
	if err := db.SetJobCLIResourceOverrides(database, jobID, &db.CLIResourceOverrides{MaxSpendCents: &lowerCents}); err != nil {
		t.Fatal(err)
	}
	previousStatus := daemonStatusFunc
	daemonStatusFunc = func(daemoncontrol.Paths) (daemoncontrol.Status, error) {
		return daemoncontrol.Status{Live: true}, nil
	}
	t.Cleanup(func() { daemonStatusFunc = previousStatus })
	result := submitControlFixture(t, transport, signers["plan-owner"], jobID, "restart-lower-cap", edge.ControlRestart, nil)

	if pending, err := pollEdgeInboxOnce(context.Background(), database, edgeInboxDeps{
		Transport: transport, Runtime: hub, HubHost: "hub-alpha",
	}); err != nil || pending != 0 {
		t.Fatalf("poll: pending=%d err=%v", pending, err)
	}
	ack, err := edgeFetchAck(context.Background(), hub, result.Nonce)
	if err != nil || !ack.Accepted {
		t.Fatalf("restart acknowledgement=%#v err=%v", ack, err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.CLIResourceOverrides == nil || job.CLIResourceOverrides.MaxSpendCents == nil || *job.CLIResourceOverrides.MaxSpendCents != lowerCents {
		t.Fatalf("restarted job max-spend = %#v, want retained %d cents", job.CLIResourceOverrides, lowerCents)
	}
}

// This kills moving the admitted ceiling write after RequeueByID: the trigger
// rejects the new queued attempt unless the narrowed cap is already visible.
func TestEdgeControlEditInstallsSpendCeilingBeforeRequeue(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	database := db.SetupTestDB(t)
	hub, signers := edgeControlFixture(t, transport)
	jobID := recordEdgeOwnedJob(t, database, "edge-alpha-plan-owner")
	oldCents := 2000
	if err := db.SetJobCLIResourceOverrides(database, jobID, &db.CLIResourceOverrides{MaxSpendCents: &oldCents}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ?`, db.StatusKilled, time.Now().Unix(), jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
		CREATE TEMP TRIGGER require_narrowed_edge_ceiling_before_requeue
		BEFORE INSERT ON job_attempts
		WHEN NEW.status = 'queued'
		BEGIN
			SELECT CASE WHEN COALESCE(
				(SELECT json_extract(cli_overrides, '$.max_spend_cents') FROM jobs WHERE id = NEW.job_id),
				999999
			) > 500 THEN RAISE(ABORT, 'spend ceiling not installed before requeue') END;
		END`); err != nil {
		t.Fatal(err)
	}
	result := submitControlFixture(t, transport, signers["plan-owner"], jobID, "edit-retry-narrow-cap",
		edge.ControlEdit, map[string]string{"max-spend": "5", "retry": "true"})

	if pending, err := pollEdgeInboxOnce(context.Background(), database, edgeInboxDeps{
		Transport: transport, Runtime: hub, HubHost: "hub-alpha",
	}); err != nil || pending != 0 {
		t.Fatalf("poll: pending=%d err=%v", pending, err)
	}
	ack, err := edgeFetchAck(context.Background(), hub, result.Nonce)
	if err != nil || !ack.Accepted {
		t.Fatalf("edit acknowledgement=%#v err=%v", ack, err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.EffectiveStatus() != db.StatusQueued || job.CLIResourceOverrides == nil ||
		job.CLIResourceOverrides.MaxSpendCents == nil || *job.CLIResourceOverrides.MaxSpendCents != 500 {
		t.Fatalf("edited job = %#v, want queued with 500-cent cap", job)
	}
}

// This establishes at the full edge-inbox control level that a display-only
// edit names no money and therefore preserves a lower cap. It also kills the
// mutation that classifies the injected authorization ceiling as a requested
// rental-policy edit and rejects the active job.
func TestEdgeControlDisplayEditOnRunningJobPreservesLowerSpendCeiling(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	database := db.SetupTestDB(t)
	hub, signers := edgeControlFixture(t, transport)
	jobID := recordEdgeOwnedJob(t, database, "edge-alpha-plan-owner")
	lowerCents := 500
	if err := db.SetJobCLIResourceOverrides(database, jobID, &db.CLIResourceOverrides{MaxSpendCents: &lowerCents}); err != nil {
		t.Fatal(err)
	}
	markEdgeControlJobRunning(t, database, jobID)
	result := submitControlFixture(t, transport, signers["plan-owner"], jobID, "running-message-edit",
		edge.ControlEdit, map[string]string{"message": "still running"})

	if pending, err := pollEdgeInboxOnce(context.Background(), database, edgeInboxDeps{
		Transport: transport, Runtime: hub, HubHost: "hub-alpha",
	}); err != nil || pending != 0 {
		t.Fatalf("poll: pending=%d err=%v", pending, err)
	}
	ack, err := edgeFetchAck(context.Background(), hub, result.Nonce)
	if err != nil || !ack.Accepted {
		t.Fatalf("edit acknowledgement=%#v err=%v", ack, err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Description != "still running" || job.CLIResourceOverrides == nil ||
		job.CLIResourceOverrides.MaxSpendCents == nil || *job.CLIResourceOverrides.MaxSpendCents != lowerCents {
		t.Fatalf("edited job = %#v, want updated description and retained %d-cent cap", job, lowerCents)
	}
}

// This kills returning a confirmed active-job edit decision as an untyped
// error, which would leave the edge pointer pending until its TTL expires.
func TestEdgeControlNonDisplayEditOnRunningJobDrainsAsActionFailure(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	database := db.SetupTestDB(t)
	hub, signers := edgeControlFixture(t, transport)
	jobID := recordEdgeOwnedJob(t, database, "edge-alpha-plan-owner")
	markEdgeControlJobRunning(t, database, jobID)
	result := submitControlFixture(t, transport, signers["plan-owner"], jobID, "running-command-edit",
		edge.ControlEdit, map[string]string{"command": "echo changed"})

	if pending, err := pollEdgeInboxOnce(context.Background(), database, edgeInboxDeps{
		Transport: transport, Runtime: hub, HubHost: "hub-alpha",
	}); err != nil || pending != 0 {
		t.Fatalf("poll: pending=%d err=%v", pending, err)
	}
	ack, err := edgeFetchAck(context.Background(), hub, result.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Accepted || ack.ReasonCode != string(edge.ReasonActionFailed) || !strings.Contains(ack.Detail, "only display-only fields") {
		t.Fatalf("active-job edit acknowledgement = %#v", ack)
	}
	if _, err := transport.Get(context.Background(), edge.InboxKey(result.Nonce)); !errors.Is(err, edge.ErrNotFound) {
		t.Fatalf("active-job edit pointer lookup error = %v, want confirmed absent", err)
	}
}

// This kills the mutation that leaves a shared runEdit usage decision untyped,
// causing a deterministic invalid request to retry forever.
func TestEdgeControlUsageFailureDrainsAsActionFailure(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	database := db.SetupTestDB(t)
	hub, signers := edgeControlFixture(t, transport)
	jobID := recordEdgeOwnedJob(t, database, "edge-alpha-plan-owner")
	result := submitControlFixture(t, transport, signers["plan-owner"], jobID, "invalid-edit-flags",
		edge.ControlEdit, map[string]string{"retry": "true", "status": db.StatusQueued})

	if pending, err := pollEdgeInboxOnce(context.Background(), database, edgeInboxDeps{
		Transport: transport, Runtime: hub, HubHost: "hub-alpha",
	}); err != nil || pending != 0 {
		t.Fatalf("poll: pending=%d err=%v", pending, err)
	}
	ack, err := edgeFetchAck(context.Background(), hub, result.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Accepted || ack.ReasonCode != string(edge.ReasonActionFailed) || !strings.Contains(ack.Detail, "cannot combine --retry with --status") {
		t.Fatalf("usage-failure acknowledgement = %#v", ack)
	}
}

// This kills the mutation that returns a confirmed invalid state as an
// untyped retryable error, leaving the pointer permanently pending.
func TestEdgeControlStateDecisionDrainsAsActionFailure(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	database := db.SetupTestDB(t)
	hub, signers := edgeControlFixture(t, transport)
	jobID := recordEdgeOwnedJob(t, database, "edge-alpha-plan-owner")
	result := submitControlFixture(t, transport, signers["plan-owner"], jobID, "invalid-resume",
		edge.ControlResume, nil)

	if _, err := pollEdgeInboxOnce(context.Background(), database, edgeInboxDeps{
		Transport: transport, Runtime: hub, HubHost: "hub-alpha",
	}); err != nil {
		t.Fatalf("pollEdgeInboxOnce: %v", err)
	}
	ack, err := edgeFetchAck(context.Background(), hub, result.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Accepted || ack.ReasonCode != string(edge.ReasonActionFailed) || !strings.Contains(ack.Detail, "only paused jobs can be resumed") {
		t.Fatalf("invalid-state control ack = %#v", ack)
	}
	if _, err := transport.Get(context.Background(), edge.InboxKey(result.Nonce)); !errors.Is(err, edge.ErrNotFound) {
		t.Fatalf("invalid-state pointer lookup error = %v, want confirmed absent", err)
	}
}

// This kills the mutation that applies running-process pause semantics to the
// CLI pause action, which must move a queued job to draft.
func TestEdgeControlPauseQueuesJobAsDraft(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	database := db.SetupTestDB(t)
	hub, signers := edgeControlFixture(t, transport)
	jobID := recordEdgeOwnedJob(t, database, "edge-alpha-plan-owner")
	result := submitControlFixture(t, transport, signers["plan-owner"], jobID, "pause-to-draft", edge.ControlPause, nil)

	if pending, err := pollEdgeInboxOnce(context.Background(), database, edgeInboxDeps{
		Transport: transport, Runtime: hub, HubHost: "hub-alpha",
	}); err != nil || pending != 0 {
		t.Fatalf("poll: pending=%d err=%v", pending, err)
	}
	ack, err := edgeFetchAck(context.Background(), hub, result.Nonce)
	if err != nil || !ack.Accepted {
		t.Fatalf("pause acknowledgement=%#v err=%v", ack, err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil || job == nil || job.EffectiveStatus() != db.StatusDraft {
		t.Fatalf("paused job=%#v err=%v, want exact draft status", job, err)
	}
}

// This kills the mutation that routes fact payloads to the jobs database or
// drops them after admission instead of writing the separate bug ledger.
func TestEdgeBugReportAndNoteUseBugDatabase(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	jobsDB := db.SetupTestDB(t)
	bugDB := db.SetupTestBugDB(t)
	hub, signers := edgeControlFixture(t, transport)

	reportPayload, _ := edge.EncodeWeftBugReportPayload(edge.WeftBugReportPayload{
		ReportID: "report-identity", Action: edge.BugReportCreate,
		Title: "edge invariant", Fingerprint: "edge-invariant", Detail: "runtime detail",
	})
	report, err := edge.Submit(context.Background(), transport, signers["plan-owner"], edge.SubmitRequest{
		SubmitterHost: "edge-alpha", Kind: edge.KindWeftBugReport, Payload: reportPayload,
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pollEdgeInboxOnce(context.Background(), jobsDB, edgeInboxDeps{Transport: transport, Runtime: hub, HubHost: "hub-alpha"}); err != nil {
		t.Fatal(err)
	}
	reportAck, err := edgeFetchAck(context.Background(), hub, report.Nonce)
	if err != nil || !reportAck.Accepted || !strings.Contains(reportAck.Detail, "wb1") {
		t.Fatalf("report ack=%#v err=%v", reportAck, err)
	}

	notePayload, _ := edge.EncodeWeftBugReportPayload(edge.WeftBugReportPayload{
		ReportID: "note-identity", Action: edge.BugReportNote, BugID: "wb1", Note: "more evidence",
	})
	if _, err := edge.Submit(context.Background(), transport, signers["plan-owner"], edge.SubmitRequest{
		SubmitterHost: "edge-alpha", Kind: edge.KindWeftBugReport, Payload: notePayload,
	}, time.Now().UTC().Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := pollEdgeInboxOnce(context.Background(), jobsDB, edgeInboxDeps{Transport: transport, Runtime: hub, HubHost: "hub-alpha"}); err != nil {
		t.Fatal(err)
	}
	bugs, err := db.ListBugs(bugDB, true)
	if err != nil || len(bugs) != 1 || bugs[0].Title != "edge invariant" {
		t.Fatalf("bugs=%#v err=%v", bugs, err)
	}
	notes, err := db.ListBugNotes(bugDB, bugs[0].ID)
	if err != nil || len(notes) != 1 || notes[0].Body != "more evidence" {
		t.Fatalf("notes=%#v err=%v", notes, err)
	}
}

// This kills the mutation that returns the closed-fingerprint decision as an
// untyped error, which would retain the never-expiring pointer forever.
func TestEdgeClosedBugFingerprintDrainsAsActionFailure(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	jobsDB := db.SetupTestDB(t)
	bugDB := db.SetupTestBugDB(t)
	bug, _, err := db.ReportBug(bugDB, db.BugReport{Title: "closed edge invariant", Fingerprint: "closed-edge-invariant"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CloseBug(bugDB, bug.ID, "fixed"); err != nil {
		t.Fatal(err)
	}
	hub, signers := edgeControlFixture(t, transport)
	payload, err := edge.EncodeWeftBugReportPayload(edge.WeftBugReportPayload{
		ReportID: "closed-report", Action: edge.BugReportCreate,
		Title: "closed edge invariant", Fingerprint: "closed-edge-invariant",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := edge.Submit(context.Background(), transport, signers["plan-owner"], edge.SubmitRequest{
		SubmitterHost: "edge-alpha", Kind: edge.KindWeftBugReport, Payload: payload,
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	if pending, err := pollEdgeInboxOnce(context.Background(), jobsDB, edgeInboxDeps{
		Transport: transport, Runtime: hub, HubHost: "hub-alpha",
	}); err != nil || pending != 0 {
		t.Fatalf("poll: pending=%d err=%v", pending, err)
	}
	ack, err := edgeFetchAck(context.Background(), hub, result.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Accepted || ack.ReasonCode != string(edge.ReasonActionFailed) || ack.Phase != "execution" || !strings.Contains(ack.Detail, "is closed") {
		t.Fatalf("closed-fingerprint acknowledgement = %#v", ack)
	}
	if _, err := transport.Get(context.Background(), edge.InboxKey(result.Nonce)); !errors.Is(err, edge.ErrNotFound) {
		t.Fatalf("closed-fingerprint pointer lookup error = %v, want confirmed absent", err)
	}
}

// This kills the mutation that treats confirmed absence of a bug as an
// unknown database failure and retains its note pointer.
func TestEdgeMissingBugNoteDrainsAsActionFailure(t *testing.T) {
	transport, err := edge.NewFSTransport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	jobsDB := db.SetupTestDB(t)
	db.SetupTestBugDB(t)
	hub, signers := edgeControlFixture(t, transport)
	payload, err := edge.EncodeWeftBugReportPayload(edge.WeftBugReportPayload{
		ReportID: "missing-note", Action: edge.BugReportNote, BugID: "wb404", Note: "evidence",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := edge.Submit(context.Background(), transport, signers["plan-owner"], edge.SubmitRequest{
		SubmitterHost: "edge-alpha", Kind: edge.KindWeftBugReport, Payload: payload,
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	if pending, err := pollEdgeInboxOnce(context.Background(), jobsDB, edgeInboxDeps{
		Transport: transport, Runtime: hub, HubHost: "hub-alpha",
	}); err != nil || pending != 0 {
		t.Fatalf("poll: pending=%d err=%v", pending, err)
	}
	ack, err := edgeFetchAck(context.Background(), hub, result.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Accepted || ack.ReasonCode != string(edge.ReasonActionFailed) || ack.Detail != "bug wb404 not found" {
		t.Fatalf("missing-bug acknowledgement = %#v", ack)
	}
}
