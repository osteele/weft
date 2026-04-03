package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeGraceStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	deleted []string
	autoAck func(key string, body []byte) (string, []byte)
}

func (s *fakeGraceStore) PutObject(_ context.Context, key string, body io.Reader, _ string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objects == nil {
		s.objects = make(map[string][]byte)
	}
	s.objects[key] = data
	if s.autoAck != nil {
		if ackKey, ackData := s.autoAck(key, data); ackKey != "" {
			s.objects[ackKey] = ackData
		}
	}
	return nil
}

func (s *fakeGraceStore) GetObject(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, ErrControlObjectNotFound
	}
	return append([]byte(nil), data...), nil
}

func (s *fakeGraceStore) DeleteObject(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	s.deleted = append(s.deleted, key)
	return nil
}

func TestSendGraceExtendWaitsForAckAndDeletesAck(t *testing.T) {
	store := &fakeGraceStore{}
	store.autoAck = func(key string, _ []byte) (string, []byte) {
		requestID := strings.TrimSuffix(strings.TrimPrefix(key, GraceExtendPrefix(42)), ".txt")
		ack := GraceCommandAck{
			RequestID:  requestID,
			Kind:       GraceCommandExtend,
			ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano),
			Accepted:   true,
			Message:    "ok",
		}
		data, _ := json.Marshal(ack)
		return GraceCommandAckKey(42, requestID), data
	}

	ack, err := SendGraceExtend(context.Background(), store, 42, 15*time.Minute)
	if err != nil {
		t.Fatalf("SendGraceExtend: %v", err)
	}
	if ack == nil || !ack.Accepted {
		t.Fatalf("ack = %+v, want accepted", ack)
	}
	if _, err := store.GetObject(context.Background(), GraceCommandAckKey(42, ack.RequestID)); err == nil {
		t.Fatalf("ack key %q should be deleted after read", GraceCommandAckKey(42, ack.RequestID))
	}
}

func TestSendGraceReleaseReturnsRejectedAck(t *testing.T) {
	store := &fakeGraceStore{}
	store.autoAck = func(key string, _ []byte) (string, []byte) {
		requestID := strings.TrimSuffix(strings.TrimPrefix(key, GraceReleasePrefix(7)), ".txt")
		ack := GraceCommandAck{
			RequestID:  requestID,
			Kind:       GraceCommandRelease,
			ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano),
			Accepted:   false,
			Message:    "instance is already terminating",
		}
		data, _ := json.Marshal(ack)
		return GraceCommandAckKey(7, requestID), data
	}

	_, err := SendGraceRelease(context.Background(), store, 7)
	if err == nil {
		t.Fatal("expected rejected grace release to return an error")
	}
	if !strings.Contains(err.Error(), "already terminating") {
		t.Fatalf("error = %v, want rejection message", err)
	}
}

func TestWaitForGraceCommandAckHonorsPollIntervalFromContext(t *testing.T) {
	store := &fakeGraceStore{objects: make(map[string][]byte)}
	const (
		instanceID = int64(9)
		requestID  = "req-123"
	)
	ackKey := GraceCommandAckKey(instanceID, requestID)

	go func() {
		time.Sleep(120 * time.Millisecond)
		ack := GraceCommandAck{
			RequestID:  requestID,
			Kind:       GraceCommandJobs,
			ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano),
			Accepted:   true,
		}
		data, _ := json.Marshal(ack)
		store.mu.Lock()
		store.objects[ackKey] = data
		store.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	ctx = WithGraceAckPollInterval(ctx, 50*time.Millisecond)

	ack, err := WaitForGraceCommandAck(ctx, store, instanceID, requestID)
	if err != nil {
		t.Fatalf("WaitForGraceCommandAck: %v", err)
	}
	if ack == nil || ack.RequestID != requestID {
		t.Fatalf("ack = %+v, want request %q", ack, requestID)
	}
}

func TestWaitForGraceCommandAckUsesDefaultPollInterval(t *testing.T) {
	store := &fakeGraceStore{objects: make(map[string][]byte)}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_, err := WaitForGraceCommandAck(ctx, store, 1, "req-default")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}
