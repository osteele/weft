package coordinatorrelay

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeAckStore struct {
	data       map[string][]byte
	getErr     error
	deletedKey string
}

func (s *fakeAckStore) GetObject(_ context.Context, key string) ([]byte, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	data, ok := s.data[key]
	if !ok {
		return nil, ErrAckNotFound
	}
	return data, nil
}

func (s *fakeAckStore) DeleteObject(_ context.Context, key string) error {
	s.deletedKey = key
	delete(s.data, key)
	return nil
}

func TestWaitForAckDeletesAckAfterRead(t *testing.T) {
	ack := Ack{
		RequestID:   "req-1",
		ProcessedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Accepted:    true,
		Message:     "ok",
	}
	data, err := json.Marshal(ack)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	store := &fakeAckStore{
		data: map[string][]byte{
			"coordinator/v1/acks/req-1.json": data,
		},
	}

	got, err := waitForAck(context.Background(), store, "req-1")
	if err != nil {
		t.Fatalf("waitForAck: %v", err)
	}
	if got == nil || !got.Accepted {
		t.Fatalf("ack = %+v, want accepted", got)
	}
	if store.deletedKey != "coordinator/v1/acks/req-1.json" {
		t.Fatalf("deletedKey = %q", store.deletedKey)
	}
}

func TestWaitForAckReturnsBackendErrorsImmediately(t *testing.T) {
	store := &fakeAckStore{getErr: errors.New("permission denied")}
	_, err := waitForAck(context.Background(), store, "req-2")
	if err == nil {
		t.Fatal("expected waitForAck to return backend error")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error = %v, want backend failure", err)
	}
}
