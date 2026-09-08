package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/edge"
	"github.com/osteele/weft/internal/ops"
	"github.com/spf13/cobra"
)

type filesystemSourceWriter struct {
	transport edge.Transport
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

func (w filesystemSourceWriter) PutObject(ctx context.Context, key string, body io.Reader, _ string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	return w.transport.Put(ctx, key, data)
}

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
		SpendCeilingUSD: 5, TargetConstraints: edge.TargetConstraints{Hosts: []string{"host-alpha"}},
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
	if err := db.PopulateEdgeSubmissionProvenance(database, []*db.Job{job}); err != nil {
		t.Fatalf("PopulateEdgeSubmissionProvenance: %v", err)
	}
	jobList, err := db.ListJobs(database, "", "", 0, nil, "all")
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	inList := false
	for _, listed := range jobList {
		if listed.ID == jobID {
			inList = true
			break
		}
	}
	if !inList {
		t.Fatalf("admitted job %d is absent from the hub job list", jobID)
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
	if job.Host != "host-alpha" {
		t.Errorf("placement host = %q, want host-alpha", job.Host)
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
	if !ack.Accepted || ack.JobID != jobID || ack.Phase != "placed" {
		t.Errorf("ack = %#v, want accepted placed job %d", ack, jobID)
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
