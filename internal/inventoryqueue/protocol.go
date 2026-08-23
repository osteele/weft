// Package inventoryqueue defines the R2 protocol used by inventory hosts that
// can reach R2 but cannot accept direct SSH connections from the controller.
package inventoryqueue

import (
	"fmt"
	"path"
	"regexp"
	"time"

	"github.com/osteele/weft/internal/opsqueue"
)

const Version = "v1"

var hostPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Request is one at-least-once queue command addressed to one inventory host.
type Request struct {
	Version   string                `json:"version"`
	RequestID string                `json:"request_id"`
	Host      string                `json:"host"`
	CreatedAt time.Time             `json:"created_at"`
	Command   opsqueue.QueueCommand `json:"command"`
}

// Ack records that the daemon durably appended a request to its local command
// log. It is diagnostic evidence; controller dispatch does not wait for it.
type Ack struct {
	Version     string    `json:"version"`
	RequestID   string    `json:"request_id"`
	Host        string    `json:"host"`
	ProcessedAt time.Time `json:"processed_at"`
}

// State is the latest runner state published by the host daemon.
type State struct {
	Version      string               `json:"version"`
	Host         string               `json:"host"`
	AgentVersion string               `json:"agent_version,omitempty"`
	UpdatedAt    time.Time            `json:"updated_at"`
	Runner       opsqueue.RunnerState `json:"runner"`
}

func validateHost(host string) error {
	if !hostPattern.MatchString(host) {
		return fmt.Errorf("invalid inventory queue host %q", host)
	}
	return nil
}

func hostPrefix(host string) (string, error) {
	if err := validateHost(host); err != nil {
		return "", err
	}
	return path.Join("inventory", Version, "hosts", host), nil
}

func InboxPrefix(host string) (string, error) {
	prefix, err := hostPrefix(host)
	if err != nil {
		return "", err
	}
	return prefix + "/inbox/", nil
}

func RequestKey(host, requestID string) (string, error) {
	if !hostPattern.MatchString(requestID) {
		return "", fmt.Errorf("invalid inventory queue request id %q", requestID)
	}
	prefix, err := InboxPrefix(host)
	if err != nil {
		return "", err
	}
	return prefix + requestID + ".json", nil
}

func AckKey(host, requestID string) (string, error) {
	if !hostPattern.MatchString(requestID) {
		return "", fmt.Errorf("invalid inventory queue request id %q", requestID)
	}
	prefix, err := hostPrefix(host)
	if err != nil {
		return "", err
	}
	return path.Join(prefix, "acks", requestID+".json"), nil
}

func StateKey(host string) (string, error) {
	prefix, err := hostPrefix(host)
	if err != nil {
		return "", err
	}
	return path.Join(prefix, "state.json"), nil
}
