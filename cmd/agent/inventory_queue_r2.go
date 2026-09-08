package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/inventoryqueue"
	"github.com/osteele/weft/internal/opsqueue"
)

const inventoryQueuePollInterval = 2 * time.Second

type inventoryQueueR2Bridge struct {
	bucket       string
	host         string
	commandsFile string
	stateFile    string
	agentVersion string
	now          func() time.Time
	get          func(string, string) (string, error)
	put          func(string, string, string) error
	list         func(string, string) ([]string, error)
	delete       func(string, string) error
}

func startInventoryQueueR2(bucket, host, queueDir, agentVersion string) func() {
	bridge := &inventoryQueueR2Bridge{
		bucket:       bucket,
		host:         host,
		commandsFile: filepath.Join(queueDir, opsqueue.CommandsFileName()),
		stateFile:    filepath.Join(queueDir, opsqueue.StateFileName()),
		agentVersion: agentVersion,
		now:          time.Now,
		get:          r2Get,
		put:          r2Put,
		list:         r2List,
		delete:       r2Delete,
	}
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(inventoryQueuePollInterval)
		defer ticker.Stop()
		for {
			if err := bridge.runOnce(); err != nil {
				fmt.Fprintf(os.Stderr, "inventory R2 queue: %v\n", err)
			}
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { close(stop) }
}

func (b *inventoryQueueR2Bridge) runOnce() error {
	var failures []string
	if err := b.publishState(); err != nil {
		failures = append(failures, "publish state: "+err.Error())
	}
	prefix, err := inventoryqueue.InboxPrefix(b.host)
	if err != nil {
		return err
	}
	names, err := b.list(b.bucket, prefix)
	if err != nil {
		failures = append(failures, "list inbox: "+err.Error())
		return fmt.Errorf("inventory queue R2 operations: %s", strings.Join(failures, "; "))
	}
	for _, name := range names {
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		key := prefix + path.Base(name)
		if err := b.processRequest(key); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", path.Base(name), err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("inventory queue R2 operations: %s", strings.Join(failures, "; "))
	}
	return nil
}

func (b *inventoryQueueR2Bridge) processRequest(key string) error {
	data, err := b.get(b.bucket, key)
	if err != nil {
		return err
	}
	if data == "" {
		return nil
	}
	var request inventoryqueue.Request
	if err := json.Unmarshal([]byte(data), &request); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	if request.Version != inventoryqueue.Version {
		return fmt.Errorf("unsupported version %q", request.Version)
	}
	if request.Host != b.host {
		return fmt.Errorf("request addressed to %q", request.Host)
	}
	wantKey, err := inventoryqueue.RequestKey(b.host, request.RequestID)
	if err != nil {
		return err
	}
	if key != wantKey {
		return fmt.Errorf("request id does not match object key")
	}
	ackKey, err := inventoryqueue.AckKey(b.host, request.RequestID)
	if err != nil {
		return err
	}
	ackData, err := b.get(b.bucket, ackKey)
	if err != nil {
		return fmt.Errorf("check ack: %w", err)
	}
	acknowledged := false
	if ackData != "" {
		var ack inventoryqueue.Ack
		if err := json.Unmarshal([]byte(ackData), &ack); err == nil &&
			ack.Version == inventoryqueue.Version && ack.RequestID == request.RequestID && ack.Host == b.host {
			acknowledged = true
		}
	}
	if !acknowledged {
		if err := opsqueue.AppendCommandLocal(b.commandsFile, request.Command); err != nil {
			return fmt.Errorf("append local command: %w", err)
		}
		ack := inventoryqueue.Ack{
			Version: inventoryqueue.Version, RequestID: request.RequestID,
			Host: b.host, ProcessedAt: b.now().UTC(),
		}
		encoded, err := json.Marshal(ack)
		if err != nil {
			return fmt.Errorf("encode ack: %w", err)
		}
		if err := b.put(b.bucket, ackKey, string(encoded)); err != nil {
			return fmt.Errorf("write ack: %w", err)
		}
	}
	if err := b.delete(b.bucket, key); err != nil {
		return fmt.Errorf("delete request: %w", err)
	}
	return nil
}

func (b *inventoryQueueR2Bridge) publishState() error {
	state := opsqueue.RunnerState{}
	if data, err := os.ReadFile(b.stateFile); err == nil {
		if err := json.Unmarshal(data, &state); err != nil {
			return fmt.Errorf("decode runner state: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read runner state: %w", err)
	}
	logDir := filepath.Clean(filepath.Join(filepath.Dir(b.stateFile), "..", "logs"))
	for idText, running := range state.Running {
		jobID, err := strconv.ParseInt(idText, 10, 64)
		if err != nil {
			continue
		}
		running.StatusFile = statusFileObservation(logDir, jobID, running.RunID)
		running.Process = processObservation(logDir, jobID)
		state.Running[idText] = running
	}
	state.PendingPayloads, state.PendingPayloadInventoryComplete, state.PendingPayloadInventoryError =
		observePendingPayloadInventory(filepath.Dir(b.stateFile))
	envelope := inventoryqueue.State{
		Version: inventoryqueue.Version, Host: b.host,
		AgentVersion: b.agentVersion, UpdatedAt: b.now().UTC(), Runner: state,
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("encode runner state: %w", err)
	}
	key, err := inventoryqueue.StateKey(b.host)
	if err != nil {
		return err
	}
	return b.put(b.bucket, key, string(encoded))
}

func observePendingPayloadInventory(queueDir string) (map[string]opsqueue.RunnerPayloadState, bool, string) {
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		return nil, false, fmt.Sprintf("read queue directory: %v", err)
	}
	payloads := make(map[string]opsqueue.RunnerPayloadState)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "job-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		idText := strings.TrimSuffix(strings.TrimPrefix(name, "job-"), ".json")
		jobID, err := strconv.ParseInt(idText, 10, 64)
		if err != nil || jobID <= 0 {
			continue
		}
		observation := opsqueue.RunnerPayloadState{Observation: opsqueue.ObservationUnknown}
		data, err := os.ReadFile(filepath.Join(queueDir, name))
		if err != nil {
			observation.Detail = fmt.Sprintf("read payload: %v", err)
			payloads[idText] = observation
			continue
		}
		var job opsqueue.CommandJob
		if err := json.Unmarshal(data, &job); err != nil {
			observation.Detail = "payload JSON is unreadable"
			payloads[idText] = observation
			continue
		}
		if job.ID != jobID {
			observation.Detail = "payload job id does not match filename"
			payloads[idText] = observation
			continue
		}
		if job.RunID <= 0 {
			observation.Detail = "payload run_id is missing"
			payloads[idText] = observation
			continue
		}
		observation.Observation = opsqueue.ObservationPresent
		observation.RunID = job.RunID
		payloads[idText] = observation
	}
	return payloads, true, ""
}
