package services

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/osteele/weft/internal/intent"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ssh"
)

// RetryItem holds a deferred intent and its target host.
type RetryItem struct {
	Intent *intent.Intent
	Host   string
	Added  time.Time
}

// RetryQueue holds intents that could not be dispatched because the target host was offline.
type RetryQueue struct {
	mu    sync.Mutex
	items []*RetryItem
}

// NewRetryQueue creates a new retry queue.
func NewRetryQueue() *RetryQueue {
	return &RetryQueue{}
}

// Add enqueues an intent for retry when the host comes back online.
func (q *RetryQueue) Add(i *intent.Intent, host string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, &RetryItem{
		Intent: i,
		Host:   host,
		Added:  time.Now(),
	})
}

// DrainForHost removes and returns all retry items for the given host.
func (q *RetryQueue) DrainForHost(host string) []*RetryItem {
	q.mu.Lock()
	defer q.mu.Unlock()

	var drained []*RetryItem
	var remaining []*RetryItem
	for _, item := range q.items {
		if item.Host == host {
			drained = append(drained, item)
		} else {
			remaining = append(remaining, item)
		}
	}
	q.items = remaining
	return drained
}

// Len returns the number of items in the retry queue.
func (q *RetryQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// DispatchFunc is called to dispatch a retried intent. Returns the job ID or an error.
type DispatchFunc func(i *intent.Intent, host string) (int64, error)

// RetryDrainer periodically attempts to dispatch deferred intents for hosts
// that have come back online.
type RetryDrainer struct {
	queue     *RetryQueue
	hostState *HostStateManager
	dispatch  DispatchFunc
	logger    *log.Logger
	interval  time.Duration
}

// NewRetryDrainer creates a new retry drainer service.
func NewRetryDrainer(queue *RetryQueue, hostState *HostStateManager, dispatch DispatchFunc, logger *log.Logger, interval time.Duration) *RetryDrainer {
	return &RetryDrainer{
		queue:     queue,
		hostState: hostState,
		dispatch:  dispatch,
		logger:    logger,
		interval:  interval,
	}
}

// Start runs the drainer loop until the context is cancelled.
func (d *RetryDrainer) Start(ctx context.Context) {
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.Drain()
		}
	}
}

// Drain attempts to dispatch deferred intents for online hosts.
func (d *RetryDrainer) Drain() {
	if d.queue.Len() == 0 {
		return
	}

	onlineHosts := d.hostState.OnlineHosts()
	for _, host := range onlineHosts {
		items := d.queue.DrainForHost(host)
		for _, item := range items {
			d.logger.Printf("retrying intent %s on %s", item.Intent.IntentID, host)
			oplog.Log(oplog.OpCoordinatorRetry, oplog.WithHost(host), oplog.WithDetailf("intent=%s", item.Intent.IntentID))

			jobID, err := d.dispatch(item.Intent, host)
			if err != nil {
				if isSSHConnectionError(err) {
					d.queue.Add(item.Intent, host)
					d.hostState.MarkOffline(host)
					break // stop retrying this host
				}
				d.logger.Printf("retry dispatch intent %s: %v", item.Intent.IntentID, err)
				oplog.Log(oplog.OpCoordinatorError, oplog.WithHost(host), oplog.WithDetailf("retry intent=%s", item.Intent.IntentID), oplog.WithError(err))
				continue
			}
			d.logger.Printf("retry dispatched intent %s as job %d on %s", item.Intent.IntentID, jobID, host)
			oplog.LogJob(oplog.OpCoordinatorDispatch, jobID, host, oplog.WithDetailf("retry intent=%s", item.Intent.IntentID))
		}
	}
}

func isSSHConnectionError(err error) bool {
	if err == nil {
		return false
	}
	return ssh.IsConnectionError(err.Error())
}
