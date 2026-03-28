package llm

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/db"
)

// DefaultPromptTemplate is the prompt template for generating descriptions
const DefaultPromptTemplate = `Describe what this shell command does in under 10 words.
Focus on the specific task, model names, or data being processed.
Avoid generic phrases like "run script" or "execute command".
Output ONLY the description, nothing else.

Command: %s

Description:`

// DescriptionGenerator handles background generation of job descriptions
type DescriptionGenerator struct {
	client     Generator
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

// GeneratorOption configures the DescriptionGenerator
type GeneratorOption func(*DescriptionGenerator)

// WithBatchSize sets the number of jobs to process per batch
func WithBatchSize(size int) GeneratorOption {
	return func(g *DescriptionGenerator) {
		g.batchSize = size
	}
}

// WithInterval sets the interval between batches
func WithInterval(interval time.Duration) GeneratorOption {
	return func(g *DescriptionGenerator) {
		g.interval = interval
	}
}

// WithModel sets the ollama model to use (only applies to Ollama backend)
func WithModel(model string) GeneratorOption {
	return func(g *DescriptionGenerator) {
		g.client = NewOllamaClient(OllamaConfigWithModel(model))
	}
}

// WithOnGenerate sets a callback function called when a description is generated
func WithOnGenerate(fn func(jobID int64, description string)) GeneratorOption {
	return func(g *DescriptionGenerator) {
		g.onGenerate = fn
	}
}

// NewGenerator creates a new background description generator
func NewGenerator(database *sql.DB, opts ...GeneratorOption) *DescriptionGenerator {
	ctx, cancel := context.WithCancel(context.Background())
	g := &DescriptionGenerator{
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
func (g *DescriptionGenerator) Start() bool {
	g.runningMu.Lock()
	defer g.runningMu.Unlock()

	if g.running {
		return false
	}

	// Check if LLM backend is available before starting
	if !g.client.IsAvailable() {
		return false
	}

	g.running = true
	g.wg.Add(1)
	go g.run()
	return true
}

// Stop stops the background generation process
func (g *DescriptionGenerator) Stop() {
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
func (g *DescriptionGenerator) IsRunning() bool {
	g.runningMu.Lock()
	defer g.runningMu.Unlock()
	return g.running
}

// run is the main loop for the background generator
func (g *DescriptionGenerator) run() {
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
func (g *DescriptionGenerator) processBatch() {
	if !g.client.IsAvailable() {
		return
	}

	jobs, err := db.GetJobsNeedingDescriptions(g.db, g.batchSize)
	if err != nil {
		slog.Warn("error getting jobs for description generation", "component", "llm", "error", err)
		return
	}

	for _, job := range jobs {
		select {
		case <-g.ctx.Done():
			return
		default:
		}

		description, hash, err := g.generateDescription(job.Command)
		if err != nil {
			slog.Warn("error generating description", "component", "llm", "job_id", job.ID, "error", err)
			return // stop batch — likely all jobs will fail with the same error
		}

		if err := db.UpdateJobGeneratedDescription(g.db, job.ID, description, hash); err != nil {
			slog.Warn("error updating job description", "component", "llm", "job_id", job.ID, "error", err)
			continue
		}

		// Call the callback if set
		if g.onGenerate != nil {
			g.onGenerate(job.ID, description)
		}
	}
}

// generateDescription generates a description for a shell command using the LLM backend
func (g *DescriptionGenerator) generateDescription(command string) (description string, hash string, err error) {
	prompt := fmt.Sprintf(DefaultPromptTemplate, command)

	ctx, cancel := context.WithTimeout(g.ctx, 30*time.Second)
	defer cancel()

	response, err := g.client.Generate(ctx, prompt)
	if err != nil {
		return "", "", err
	}

	// Clean up the response - remove newlines and extra whitespace
	description = strings.TrimSpace(response)
	description = strings.ReplaceAll(description, "\n", " ")
	description = strings.Join(strings.Fields(description), " ")

	// Truncate if too long (max 100 chars for display)
	if len(description) > 100 {
		description = description[:97] + "..."
	}

	hash = g.client.GenerationHash(prompt)

	return description, hash, nil
}

// GenerateOne generates a description for a single job synchronously
// Returns the generated description and hash, or an error
func (g *DescriptionGenerator) GenerateOne(job *db.Job) (string, string, error) {
	return g.generateDescription(job.Command)
}

// Client returns the underlying LLM client
func (g *DescriptionGenerator) Client() Generator {
	return g.client
}

// GenerateText generates text from a raw prompt (convenience method for host summaries etc.)
func (g *DescriptionGenerator) GenerateText(prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(g.ctx, 60*time.Second)
	defer cancel()
	return g.client.Generate(ctx, prompt)
}

// IsAvailable returns whether the underlying LLM backend is available
func (g *DescriptionGenerator) IsAvailable() bool {
	return g.client.IsAvailable()
}
