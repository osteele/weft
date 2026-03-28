// Package retry provides a thin wrapper around cenkalti/backoff/v4
// for consistent retry policies across the weft codebase.
package retry

import (
	"context"
	"time"

	"github.com/cenkalti/backoff/v4"
)

// Option configures retry behavior.
type Option func(*config)

type config struct {
	retryIf func(error) bool
	onRetry func(attempt int, err error, delay time.Duration)
}

// WithRetryIf filters which errors are retryable. Errors where pred
// returns false are treated as permanent and returned immediately.
func WithRetryIf(pred func(error) bool) Option {
	return func(c *config) { c.retryIf = pred }
}

// WithOnRetry is called before each retry sleep with the attempt number
// (1-based, where 1 means the first retry), the error, and the upcoming delay.
func WithOnRetry(fn func(attempt int, err error, delay time.Duration)) Option {
	return func(c *config) { c.onRetry = fn }
}

// Do retries op until it succeeds, the backoff is exhausted, or ctx is cancelled.
func Do(ctx context.Context, b backoff.BackOff, op func() error, opts ...Option) error {
	_, err := DoVal(ctx, b, func() (struct{}, error) {
		return struct{}{}, op()
	}, opts...)
	return err
}

// DoVal retries op and returns the value from the first successful call.
func DoVal[T any](ctx context.Context, b backoff.BackOff, op func() (T, error), opts ...Option) (T, error) {
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}

	wrapped := func() (T, error) {
		val, err := op()
		if err != nil && cfg.retryIf != nil && !cfg.retryIf(err) {
			return val, backoff.Permanent(err)
		}
		return val, err
	}

	var notify backoff.Notify
	if cfg.onRetry != nil {
		retryNum := 0
		notify = func(err error, d time.Duration) {
			retryNum++
			cfg.onRetry(retryNum, err, d)
		}
	}

	return backoff.RetryNotifyWithData(wrapped, backoff.WithContext(b, ctx), notify)
}

// FixedAttempts returns a backoff that retries with a constant interval
// for up to n total attempts (n-1 retries).
func FixedAttempts(n int, interval time.Duration) backoff.BackOff {
	return &fixedAttempts{max: n, interval: interval}
}

type fixedAttempts struct {
	max      int
	interval time.Duration
	attempt  int
}

func (f *fixedAttempts) NextBackOff() time.Duration {
	f.attempt++
	if f.attempt >= f.max {
		return backoff.Stop
	}
	return f.interval
}

func (f *fixedAttempts) Reset() { f.attempt = 0 }

// ConstantWithDeadline returns a constant-interval backoff that stops
// after maxElapsed time has passed.
func ConstantWithDeadline(interval, maxElapsed time.Duration) backoff.BackOff {
	b := &backoff.ExponentialBackOff{
		InitialInterval:     interval,
		Multiplier:          1.0,
		RandomizationFactor: 0,
		MaxInterval:         interval,
		MaxElapsedTime:      maxElapsed,
		Clock:               backoff.SystemClock,
	}
	b.Reset()
	return b
}

// ExplicitDelays returns a backoff that uses the given sequence of delays.
// After exhausting the sequence, it signals stop.
func ExplicitDelays(delays ...time.Duration) backoff.BackOff {
	return &explicitDelays{delays: delays}
}

type explicitDelays struct {
	delays []time.Duration
	index  int
}

func (e *explicitDelays) NextBackOff() time.Duration {
	if e.index >= len(e.delays) {
		return backoff.Stop
	}
	d := e.delays[e.index]
	e.index++
	return d
}

func (e *explicitDelays) Reset() { e.index = 0 }
