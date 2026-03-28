package retry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
)

func TestDo_Success(t *testing.T) {
	calls := 0
	err := Do(context.Background(), FixedAttempts(3, 0), func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestDo_RetriesThenSucceeds(t *testing.T) {
	calls := 0
	err := Do(context.Background(), FixedAttempts(5, 0), func() error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestDo_ExhaustsAttempts(t *testing.T) {
	calls := 0
	err := Do(context.Background(), FixedAttempts(3, 0), func() error {
		calls++
		return errors.New("always fails")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestDo_WithRetryIf_PermanentError(t *testing.T) {
	permanent := errors.New("permanent")
	transient := errors.New("transient")
	calls := 0
	err := Do(context.Background(), FixedAttempts(5, 0), func() error {
		calls++
		if calls == 1 {
			return transient
		}
		return permanent
	}, WithRetryIf(func(err error) bool {
		return errors.Is(err, transient)
	}))
	if !errors.Is(err, permanent) {
		t.Fatalf("expected permanent error, got: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls, got %d", calls)
	}
}

func TestDo_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := Do(ctx, FixedAttempts(100, time.Millisecond), func() error {
		calls++
		if calls == 2 {
			cancel()
		}
		return errors.New("fail")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

func TestDoVal_ReturnsValue(t *testing.T) {
	calls := 0
	val, err := DoVal(context.Background(), FixedAttempts(3, 0), func() (string, error) {
		calls++
		if calls < 2 {
			return "", errors.New("transient")
		}
		return "hello", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "hello" {
		t.Fatalf("expected 'hello', got %q", val)
	}
}

func TestWithOnRetry_CallCount(t *testing.T) {
	retryAttempts := []int{}
	err := Do(context.Background(), FixedAttempts(4, 0), func() error {
		return errors.New("fail")
	}, WithOnRetry(func(attempt int, err error, delay time.Duration) {
		retryAttempts = append(retryAttempts, attempt)
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	// 4 total attempts = 3 retries
	if len(retryAttempts) != 3 {
		t.Fatalf("expected 3 retry callbacks, got %d: %v", len(retryAttempts), retryAttempts)
	}
	for i, a := range retryAttempts {
		if a != i+1 {
			t.Fatalf("retry %d: expected attempt %d, got %d", i, i+1, a)
		}
	}
}

func TestExplicitDelays_Sequence(t *testing.T) {
	b := ExplicitDelays(time.Second, 2*time.Second, 5*time.Second)
	if d := b.NextBackOff(); d != time.Second {
		t.Fatalf("expected 1s, got %v", d)
	}
	if d := b.NextBackOff(); d != 2*time.Second {
		t.Fatalf("expected 2s, got %v", d)
	}
	if d := b.NextBackOff(); d != 5*time.Second {
		t.Fatalf("expected 5s, got %v", d)
	}
	if d := b.NextBackOff(); d != backoff.Stop {
		t.Fatalf("expected Stop, got %v", d)
	}
}

func TestExplicitDelays_Reset(t *testing.T) {
	b := ExplicitDelays(time.Second)
	b.NextBackOff()
	b.Reset()
	if d := b.NextBackOff(); d != time.Second {
		t.Fatalf("expected 1s after reset, got %v", d)
	}
}

func TestFixedAttempts_Count(t *testing.T) {
	b := FixedAttempts(3, time.Second)
	// First call after first attempt
	if d := b.NextBackOff(); d != time.Second {
		t.Fatalf("expected 1s, got %v", d)
	}
	// Second call after second attempt
	if d := b.NextBackOff(); d != time.Second {
		t.Fatalf("expected 1s, got %v", d)
	}
	// Third call: should stop (3 attempts exhausted)
	if d := b.NextBackOff(); d != backoff.Stop {
		t.Fatalf("expected Stop, got %v", d)
	}
}
