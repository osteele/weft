package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/osteele/weft/internal/appdirs"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/edgeview"
	"github.com/osteele/weft/internal/secrets"
)

// edgeViewDeps are the hub-side inputs every section producer shares: the
// ledger and the loaded config. Producers read both; they never sync over the
// network — the view is a projection of the ledger as it stands.
type edgeViewDeps struct {
	DB  *sql.DB
	Cfg *config.Config
}

// edgeViewStaticProducers builds each static view section on the hub. Every
// section named in edgeMirrorServed must have an entry here (or a job-scoped
// producer); edgeview_publish_test.go pins the correspondence so a served
// command can never lack a producer. Producers register themselves with init
// functions next to their sections.
var edgeViewStaticProducers = map[string]func(ctx context.Context, deps edgeViewDeps) ([]byte, error){}

// marshalViewJSON encodes a view section the same way the hub's --json paths
// encode to stdout (two-space indent, trailing newline), so the published
// bytes are exactly what a hub-side JSON render produces.
func marshalViewJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

const edgeViewPublishDebounce = 5 * time.Second

type edgeViewPublishState struct {
	LastAttempt           time.Time         `json:"last_attempt"`
	LastSuccessfulPublish time.Time         `json:"last_successful_publish,omitempty"`
	SectionErrors         map[string]string `json:"section_errors,omitempty"`
	PublishError          string            `json:"publish_error,omitempty"`
}

func shouldPublishEdgeView(cfg *config.Config) bool {
	return cfg != nil && !cfg.Edge.IsEdge() && cfg.Edge.View.Configured()
}

func edgeViewPublishStatePath() (string, error) {
	dir, err := appdirs.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "edge-view-publisher.json"), nil
}

func loadEdgeViewPublishState() (edgeViewPublishState, error) {
	path, err := edgeViewPublishStatePath()
	if err != nil {
		return edgeViewPublishState{}, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return edgeViewPublishState{}, nil
	}
	if err != nil {
		return edgeViewPublishState{}, fmt.Errorf("read edge view publisher state: %w", err)
	}
	var state edgeViewPublishState
	if err := json.Unmarshal(data, &state); err != nil {
		return edgeViewPublishState{}, fmt.Errorf("decode edge view publisher state: %w", err)
	}
	return state, nil
}

func saveEdgeViewPublishState(state edgeViewPublishState) error {
	path, err := edgeViewPublishStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create edge view publisher state directory: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode edge view publisher state: %w", err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write edge view publisher state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace edge view publisher state: %w", err)
	}
	return nil
}

func edgeViewProducers(ctx context.Context, deps edgeViewDeps) map[string]edgeview.Producer {
	producers := make(map[string]edgeview.Producer, len(edgeViewStaticProducers))
	for name, produce := range edgeViewStaticProducers {
		produce := produce
		producers[name] = func(ctx context.Context) ([]byte, error) { return produce(ctx, deps) }
	}

	indexBytes, indexErr := produceJobsIndexSection(ctx, deps)
	producers[edgeview.SectionJobsIndex] = func(context.Context) ([]byte, error) {
		return indexBytes, indexErr
	}
	if indexErr != nil {
		return producers
	}
	var index jobListView
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		producers[edgeview.SectionJobsIndex] = func(context.Context) ([]byte, error) {
			return nil, fmt.Errorf("decode produced jobs index: %w", err)
		}
		return producers
	}
	for _, job := range index.Jobs {
		if job == nil {
			continue
		}
		jobID := job.ID
		formattedID := fmt.Sprintf("wj%d", jobID)
		producers[edgeview.JobDetailSection(formattedID)] = func(ctx context.Context) ([]byte, error) {
			return produceJobDetailSection(ctx, deps, jobID)
		}
		producers[edgeview.JobLogTailSection(formattedID)] = func(ctx context.Context) ([]byte, error) {
			return produceJobLogTailSection(ctx, deps, jobID)
		}
	}
	return producers
}

func runEdgeViewPublisher(ctx context.Context, database *sql.DB, cfg *config.Config) {
	transport, err := edgeViewTransport(cfg, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: edge view publisher unavailable: %s\n", secrets.RedactText(err.Error()))
		return
	}
	hostname, err := os.Hostname()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: edge view publisher cannot determine hub host: %s\n", err)
		return
	}
	publisher := edgeview.NewPublisher(transport, hostname, Version)
	deps := edgeViewDeps{DB: database, Cfg: cfg}
	changeSource := openDBChangeSource("edge view publisher")
	if changeSource != nil {
		defer changeSource.Close()
	}

	lastFullPublish := time.Time{}
	fullPublish := func() {
		attempt := time.Now()
		next, publishErr := publisher.Publish(ctx, edgeViewProducers(ctx, deps))
		state, stateErr := loadEdgeViewPublishState()
		if stateErr != nil {
			fmt.Fprintf(os.Stderr, "warning: load edge view publisher state: %s\n", stateErr)
		}
		state.LastAttempt = attempt
		if publishErr != nil {
			state.PublishError = secrets.RedactText(publishErr.Error())
			fmt.Fprintf(os.Stderr, "warning: publish edge view: %s\n", state.PublishError)
		} else {
			state.LastSuccessfulPublish = next.PublishedAt
			state.SectionErrors = next.Errors
			state.PublishError = ""
		}
		if err := saveEdgeViewPublishState(state); err != nil {
			fmt.Fprintf(os.Stderr, "warning: save edge view publisher state: %s\n", err)
		}
		lastFullPublish = attempt
	}
	fullPublish()
	interval := cfg.Edge.View.PublishInterval()
	nextHeartbeat := time.Now().Add(interval)
	pendingChange := false
	for ctx.Err() == nil {
		nextWake := edgeViewNextWake(lastFullPublish, nextHeartbeat, pendingChange)
		wait := time.Until(nextWake)
		changed := false
		if wait > 0 {
			var done bool
			changed, done = waitForDBChangeOrTimeout(ctx, changeSource, wait, "edge view publisher")
			if done {
				return
			}
		}
		pendingChange = pendingChange || changed
		now := time.Now()
		published := false
		if pendingChange && !now.Before(lastFullPublish.Add(edgeViewPublishDebounce)) {
			fullPublish()
			pendingChange = false
			published = true
		}
		if !now.Before(nextHeartbeat) {
			if !published {
				// A heartbeat is a real publication, not a timestamp-only
				// rewrite: log-tail sections can change without a ledger write.
				fullPublish()
			}
			nextHeartbeat = now.Add(interval)
		}
	}
}

func edgeViewNextWake(lastFullPublish, nextHeartbeat time.Time, pendingChange bool) time.Time {
	if !pendingChange {
		return nextHeartbeat
	}
	debounced := lastFullPublish.Add(edgeViewPublishDebounce)
	if debounced.Before(nextHeartbeat) {
		return debounced
	}
	return nextHeartbeat
}

func edgeViewSectionNames(manifest *edgeview.Manifest) []string {
	if manifest == nil {
		return nil
	}
	names := make([]string, 0, len(manifest.Sections))
	for name := range manifest.Sections {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
