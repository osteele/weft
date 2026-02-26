// Package intent defines the intent types used for coordinator-based job placement.
// Intents are JSON files written to the coordinator host via SSH. The coordinator
// watches for new intents, scores hosts, and dispatches jobs to remote queue runners.
package intent

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Intent represents a placement request submitted to the coordinator.
type Intent struct {
	Timestamp time.Time `json:"ts"`
	Op        string    `json:"op"`        // "place"
	IntentID  string    `json:"intent_id"` // UUID for idempotency
	Source    string    `json:"source"`    // submitter hostname
	Job       IntentJob `json:"job"`
}

// IntentJob describes the job to be placed.
type IntentJob struct {
	ID          int64             `json:"id"`
	Cmd         string            `json:"cmd"`
	Dir         string            `json:"dir"`
	Desc        string            `json:"desc,omitempty"`
	Env         []string          `json:"env,omitempty"`
	Inputs      []string          `json:"inputs,omitempty"`
	Outputs     []string          `json:"outputs,omitempty"`
	Constraints IntentConstraints `json:"constraints,omitzero"`
	Tags        []string          `json:"tags,omitempty"`
	DepSpec     string            `json:"dep_spec,omitempty"`
	GPUMemGB    *int              `json:"gpu_mem,omitempty"`
}

// IntentConstraints specifies placement constraints for the job.
type IntentConstraints struct {
	GPUClass string `json:"gpu_class,omitempty"`
	GPUMemGB int    `json:"gpu_mem_gb,omitempty"`
	Host     string `json:"host,omitempty"` // explicit host preference
}

// ParseFile reads and parses an intent JSON file.
func ParseFile(path string) (*Intent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read intent file: %w", err)
	}
	return Parse(data)
}

// Parse parses intent JSON data.
func Parse(data []byte) (*Intent, error) {
	var intent Intent
	if err := json.Unmarshal(data, &intent); err != nil {
		return nil, fmt.Errorf("parse intent: %w", err)
	}
	if err := intent.Validate(); err != nil {
		return nil, err
	}
	return &intent, nil
}

// Marshal serializes the intent to JSON.
func (i *Intent) Marshal() ([]byte, error) {
	return json.Marshal(i)
}

// Validate checks that required fields are present.
func (i *Intent) Validate() error {
	if i.IntentID == "" {
		return fmt.Errorf("intent missing intent_id")
	}
	if i.Op == "" {
		return fmt.Errorf("intent missing op")
	}
	if i.Job.Cmd == "" {
		return fmt.Errorf("intent missing job command")
	}
	return nil
}

// Filename returns the conventional filename for this intent.
func (i *Intent) Filename() string {
	return fmt.Sprintf("%d-%s.json", i.Job.ID, i.IntentID)
}
