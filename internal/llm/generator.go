package llm

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
)

// Generator handles background generation of job descriptions
type Generator struct {
	client     *Client
	db         *sql.DB
	batchSize  int
	interval   time.Duration
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	running    bool
	runningMu  sync.Mutex
	onGenerate func(jobID int64, description string) // Callback for UI updates
}

// GeneratorOption configures the Generator
type GeneratorOption func(*Generator)

// WithBatchSize sets the number of jobs to process per batch
func WithBatchSize(size int) GeneratorOption {
	return func(g *Generator) {
		g.batchSize = size
	}
}

// WithInterval sets the interval between batches
func WithInterval(interval time.Duration) GeneratorOption {
	return func(g *Generator) {
		g.interval = interval
	}
}

// WithModel sets the ollama model to use
func WithModel(model string) GeneratorOption {
	return func(g *Generator) {
		g.client = NewClient(ConfigWithModel(model))
	}
}

// WithOnGenerate sets a callback function called when a description is generated
func WithOnGenerate(fn func(jobID int64, description string)) GeneratorOption {
	return func(g *Generator) {
		g.onGenerate = fn
	}
}

// NewGenerator creates a new background description generator
func NewGenerator(database *sql.DB, opts ...GeneratorOption) *Generator {
	ctx, cancel := context.WithCancel(context.Background())
	g := &Generator{
		client:    NewDefaultClient(),
		db:        database,
		batchSize: 10,
		interval:  5 * time.Second,
		ctx:       ctx,
		cancel:    cancel,
	}

	for _, opt := range opts {
		opt(g)
	}

	return g
}

// Start begins the background generation process
// Returns immediately; generation runs in a goroutine
func (g *Generator) Start() bool {
	g.runningMu.Lock()
	defer g.runningMu.Unlock()

	if g.running {
		return false
	}

	// Check if ollama is available before starting
	if !g.client.IsAvailable() {
		return false
	}

	g.running = true
	g.wg.Add(1)
	go g.run()
	return true
}

// Stop stops the background generation process
func (g *Generator) Stop() {
	g.runningMu.Lock()
	if !g.running {
		g.runningMu.Unlock()
		return
	}
	g.runningMu.Unlock()

	g.cancel()
	g.wg.Wait()

	g.runningMu.Lock()
	g.running = false
	g.runningMu.Unlock()
}

// IsRunning returns whether the generator is currently running
func (g *Generator) IsRunning() bool {
	g.runningMu.Lock()
	defer g.runningMu.Unlock()
	return g.running
}

// run is the main loop for the background generator
func (g *Generator) run() {
	defer g.wg.Done()

	// Run first batch immediately
	g.processBatch()

	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()

	for {
		select {
		case <-g.ctx.Done():
			return
		case <-ticker.C:
			g.processBatch()
		}
	}
}

// processBatch processes a batch of jobs needing descriptions
func (g *Generator) processBatch() {
	jobs, err := db.GetJobsNeedingDescriptions(g.db, g.batchSize)
	if err != nil {
		log.Printf("llm: error getting jobs: %v", err)
		return
	}

	for _, job := range jobs {
		select {
		case <-g.ctx.Done():
			return
		default:
		}

		description, hash, err := g.client.GenerateDescription(job.Command)
		if err != nil {
			log.Printf("llm: error generating description for job %d: %v", job.ID, err)
			continue
		}

		if err := db.UpdateJobGeneratedDescription(g.db, job.ID, description, hash); err != nil {
			log.Printf("llm: error updating job %d: %v", job.ID, err)
			continue
		}

		// Call the callback if set
		if g.onGenerate != nil {
			g.onGenerate(job.ID, description)
		}
	}
}

// GenerateOne generates a description for a single job synchronously
// Returns the generated description and hash, or an error
func (g *Generator) GenerateOne(job *db.Job) (string, string, error) {
	return g.client.GenerateDescription(job.Command)
}

// Client returns the underlying LLM client
func (g *Generator) Client() *Client {
	return g.client
}
