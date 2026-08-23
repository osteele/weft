package main

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/osteele/weft/internal/cloud"
)

// midJobDrainInterval is how often the poller drains R2 jobs requests while a
// job is executing. Draining mid-job acks the request immediately, so the
// controller can confirm a pending move and stop the source without waiting
// for the current job to finish; execution of the accepted jobs still waits
// behind the current job.
const midJobDrainInterval = 45 * time.Second

// jobRequestPoller drains R2 grace jobs requests concurrently with job
// execution. drain is mutex-serialized so the background ticker and the
// loop-bottom merge never process the same R2 request twice.
type jobRequestPoller struct {
	bucket     string
	instanceID int64

	mu      sync.Mutex
	pending []cloud.AgentJob

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

func startJobRequestPoller(bucket string, instanceID int64) *jobRequestPoller {
	p := &jobRequestPoller{
		bucket:     bucket,
		instanceID: instanceID,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	fatalAgentGo("job-request-poller", func() {
		defer close(p.done)
		ticker := time.NewTicker(midJobDrainInterval)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-ticker.C:
				p.drain(nil)
			}
		}
	})
	return p
}

// drain pulls pending jobs requests from R2 into the buffer, acking each as
// it is consumed. onPhase is nil for background ticks so source-extract
// phases don't clobber the running job's phase marker.
func (p *jobRequestPoller) drain(onPhase func(string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	jobs, err := drainGraceJobRequests(p.bucket, p.instanceID, onPhase)
	if err != nil {
		fmt.Fprintf(os.Stderr, "drain job requests: %v\n", err)
		return
	}
	p.pending = append(p.pending, jobs...)
}

// Take drains R2 once more (with phase reporting) and returns everything
// accumulated since the previous Take.
func (p *jobRequestPoller) Take(onPhase func(string)) []cloud.AgentJob {
	p.drain(onPhase)
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.pending
	p.pending = nil
	return out
}

// Stop halts the background ticker and waits for any in-flight drain to
// finish. Idempotent.
func (p *jobRequestPoller) Stop() {
	p.stopOnce.Do(func() { close(p.stop) })
	<-p.done
}
