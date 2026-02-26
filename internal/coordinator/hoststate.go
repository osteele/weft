package coordinator

import (
	"sync"
	"time"

	"github.com/osteele/weft/internal/intent"
)

// HostState tracks the online/offline state of a remote host.
type HostState struct {
	Name       string
	Online     bool
	LastProbe  time.Time
	LastOnline time.Time
}

// retryQueue holds intents that could not be dispatched because the target host was offline.
type retryQueue struct {
	mu    sync.Mutex
	items []*retryItem
}

type retryItem struct {
	intent *intent.Intent
	host   string
	added  time.Time
}

func newRetryQueue() *retryQueue {
	return &retryQueue{}
}

// Add enqueues an intent for retry when the host comes back online.
func (q *retryQueue) Add(intent *intent.Intent, host string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, &retryItem{
		intent: intent,
		host:   host,
		added:  time.Now(),
	})
}

// DrainForHost removes and returns all retry items for the given host.
func (q *retryQueue) DrainForHost(host string) []*retryItem {
	q.mu.Lock()
	defer q.mu.Unlock()

	var drained []*retryItem
	var remaining []*retryItem
	for _, item := range q.items {
		if item.host == host {
			drained = append(drained, item)
		} else {
			remaining = append(remaining, item)
		}
	}
	q.items = remaining
	return drained
}

// Len returns the number of items in the retry queue.
func (q *retryQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}
