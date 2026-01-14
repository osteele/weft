# Sync Queue Architecture Design

## Current Problems

1. **Blocking sync**: TUI sets `m.syncing = true` and waits for completion. Slow connections block all syncs.
2. **No debouncing**: Every tick tries to start a sync, even if nothing changed.
3. **Sequential processing**: Jobs synced one-by-one over SSH, compounding latency.
4. **No prioritization**: All hosts treated equally regardless of activity.

## Proposed Architecture

### Core Concept: Request-Based Sync Worker

Instead of the TUI directly calling sync functions, it sends **requests** to a sync worker that manages execution.

```
┌─────────┐     requests      ┌─────────────┐     results     ┌─────────┐
│   TUI   │ ───────────────▶  │ Sync Worker │ ──────────────▶ │   TUI   │
└─────────┘   (channel)       └─────────────┘   (channel)     └─────────┘
                                    │
                                    ▼
                              ┌───────────┐
                              │  Remote   │
                              │  Hosts    │
                              └───────────┘
```

### Request Types

```go
type SyncRequest struct {
    Type     SyncRequestType
    Host     string          // Empty = all hosts
    JobID    int64           // 0 = all jobs on host
    Priority SyncPriority
}

type SyncRequestType int
const (
    SyncAll        SyncRequestType = iota  // Full sync
    SyncHost                               // Single host
    SyncJob                                // Single job
    SyncActiveOnly                         // Only active jobs
)

type SyncPriority int
const (
    PriorityLow    SyncPriority = iota
    PriorityNormal
    PriorityHigh   // User-initiated
)
```

### Sync Worker Behavior

1. **Request coalescing**: Multiple requests for same host/job within a window become one request
2. **Rate limiting**: Configurable minimum interval between syncs per host
3. **Priority queue**: High-priority requests (user-initiated) jump the queue
4. **Parallel execution**: Multiple hosts can sync concurrently
5. **Timeout handling**: Individual sync operations have timeouts; failures don't block others

### Worker Implementation Sketch

```go
type SyncWorker struct {
    requests    chan SyncRequest
    results     chan SyncResult

    // Per-host state
    hostState   map[string]*HostSyncState

    // Configuration
    minInterval time.Duration  // Min time between syncs per host
    maxParallel int            // Max concurrent host syncs
}

type HostSyncState struct {
    lastSync    time.Time
    pending     bool           // Request waiting
    inProgress  bool           // Sync running
}

func (w *SyncWorker) Run(ctx context.Context) {
    ticker := time.NewTicker(100 * time.Millisecond)
    defer ticker.Stop()

    for {
        select {
        case req := <-w.requests:
            w.enqueue(req)

        case <-ticker.C:
            w.processQueue()

        case <-ctx.Done():
            return
        }
    }
}

func (w *SyncWorker) enqueue(req SyncRequest) {
    // Coalesce: if same host already pending, merge/upgrade priority
    // Mark host as having pending request
}

func (w *SyncWorker) processQueue() {
    // For each host with pending request:
    //   - Check if minInterval elapsed since last sync
    //   - Check if under maxParallel limit
    //   - If so, start sync goroutine
}
```

### TUI Integration

```go
// In TUI model
type Model struct {
    syncWorker  *SyncWorker
    // Remove: syncing bool
    // Remove: lastSyncTime time.Time
}

// Request sync (non-blocking)
func (m Model) requestSync(host string, priority SyncPriority) tea.Cmd {
    return func() tea.Msg {
        m.syncWorker.Request(SyncRequest{
            Type:     SyncHost,
            Host:     host,
            Priority: priority,
        })
        return nil  // Fire and forget
    }
}

// Handle results
case syncResultMsg:
    // Update job list, flash messages, etc.
    return m.handleSyncResult(msg)
```

### Per-Host Rate Requests

The TUI requests different sync rates per host based on job/queue status:

```go
type HostSyncRate int
const (
    RateIdle     HostSyncRate = iota  // No active jobs: 60s
    RateQueued                         // Queued but not running: 30s
    RateRunning                        // Jobs running: 10s
    RateWarmup                         // Job just started: 5s (brief burst)
)

func (r HostSyncRate) Interval() time.Duration {
    switch r {
    case RateWarmup:  return 5 * time.Second
    case RateRunning: return 10 * time.Second
    case RateQueued:  return 30 * time.Second
    default:          return 60 * time.Second
    }
}
```

**TUI determines rate per host:**

```go
func (m Model) getHostSyncRate(host string) HostSyncRate {
    jobs := m.jobsByHost[host]

    hasRunning := false
    hasQueued := false
    hasRecentStart := false

    for _, job := range jobs {
        switch job.Status {
        case db.StatusRunning, db.StatusStarting:
            hasRunning = true
            if time.Since(job.StartTime) < 2*time.Minute {
                hasRecentStart = true
            }
        case db.StatusQueued:
            hasQueued = true
        }
    }

    if hasRecentStart {
        return RateWarmup    // Brief burst for newly started jobs
    }
    if hasRunning {
        return RateRunning   // Track running job progress
    }
    if hasQueued {
        return RateQueued    // Watch for job starts
    }
    return RateIdle          // Nothing active
}
```

**TUI sends rate requests per host:**

```go
// On tick, request sync for each host at appropriate rate
case tickMsg:
    var cmds []tea.Cmd
    for _, host := range m.hosts {
        rate := m.getHostSyncRate(host.Name)
        cmds = append(cmds, m.requestHostSync(host.Name, rate))
    }
    return m, tea.Batch(cmds...)

func (m Model) requestHostSync(host string, rate HostSyncRate) tea.Cmd {
    return func() tea.Msg {
        m.syncWorker.RequestWithRate(SyncRequest{
            Type: SyncHost,
            Host: host,
            Rate: rate,
        })
        return nil
    }
}
```

**Worker respects requested rates:**

```go
type HostSyncState struct {
    lastSync      time.Time
    requestedRate HostSyncRate  // Most recent requested rate
    pending       bool
    inProgress    bool
}

func (w *SyncWorker) shouldSync(host string) bool {
    state := w.hostState[host]
    if state.inProgress {
        return false
    }
    interval := state.requestedRate.Interval()
    return time.Since(state.lastSync) >= interval
}
```

**Benefits of per-host rates:**

1. **Responsive**: Running jobs get frequent updates
2. **Efficient**: Idle hosts don't waste bandwidth
3. **Adaptive**: Rate changes automatically as jobs start/finish
4. **Burst mode**: New jobs get extra attention during warmup

### Benefits

1. **Non-blocking**: TUI never waits for sync completion
2. **Debounced**: Multiple rapid requests coalesce into one sync
3. **Parallel**: Multiple hosts sync concurrently
4. **Prioritized**: User actions get immediate attention
5. **Resilient**: Slow/failed syncs don't block others
6. **Testable**: Worker can be tested independently

### Migration Path

1. Add SyncWorker alongside existing sync code
2. TUI sends requests to worker instead of calling performBackgroundSync
3. Worker results flow back via existing message handling
4. Remove old m.syncing lock

### Configuration

```yaml
sync:
  min_interval: 10s       # Minimum time between syncs per host
  active_interval: 10s    # Request rate when jobs active
  idle_interval: 60s      # Request rate when idle
  max_parallel: 3         # Max concurrent host syncs
  request_timeout: 30s    # Timeout per sync operation
```

## Implementation Phases

### Phase 1: Basic Worker
- Request channel
- Simple per-host rate limiting
- Results channel

### Phase 2: Coalescing
- Request deduplication
- Priority handling

### Phase 3: Parallel Execution
- Concurrent host syncs
- Semaphore for max parallelism

### Phase 4: Adaptive Rates
- Activity-based rate adjustment
- Backoff on failures
