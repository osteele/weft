package coordinator

import (
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
)

// startWatcher sets up an fsnotify watcher on the intent directory and
// sends new intent file paths to the returned channel. The watcher runs
// until the done channel is closed.
func startWatcher(intentDir string, done <-chan struct{}) (<-chan string, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	// Ensure directory exists
	if err := os.MkdirAll(intentDir, 0755); err != nil {
		watcher.Close()
		return nil, err
	}

	if err := watcher.Add(intentDir); err != nil {
		watcher.Close()
		return nil, err
	}

	ch := make(chan string, 64)

	go func() {
		defer watcher.Close()
		defer close(ch)

		for {
			select {
			case <-done:
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				// Only process Create events for .json files (not temp files)
				if event.Has(fsnotify.Create) || event.Has(fsnotify.Write) {
					name := filepath.Base(event.Name)
					if strings.HasSuffix(name, ".json") && !strings.HasPrefix(name, ".tmp.") {
						ch <- event.Name
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Printf("watcher error: %v", err)
			}
		}
	}()

	return ch, nil
}

// scanExistingIntents returns paths of existing .json files in the intent directory,
// used for catch-up processing on startup.
func scanExistingIntents(intentDir string) ([]string, error) {
	entries, err := os.ReadDir(intentDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var paths []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".json") && !strings.HasPrefix(name, ".tmp.") {
			paths = append(paths, filepath.Join(intentDir, name))
		}
	}
	return paths, nil
}
