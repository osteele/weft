package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/vastai"
)

// watchInstancesPlain prints line-oriented status updates for cloud instances.
// Suitable for non-TTY output and parsing by coding agents.
func watchInstancesPlain(database *sql.DB, instanceIDs []int64) error {
	client := vastai.NewClient()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle ctrl-c gracefully
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		cancel()
	}()

	var wg sync.WaitGroup
	for _, id := range instanceIDs {
		wg.Add(1)
		go func(instanceID int64) {
			defer wg.Done()

			ch := campaign.WatchInstance(ctx, client, database, instanceID, 2*time.Second, 10*time.Second)
			var prev campaign.InstanceUpdate

			for update := range ch {
				output := campaign.FormatPlainUpdate(prev, update)
				if output != "" {
					fmt.Println(output)
				}
				prev = update
			}
		}(id)
	}

	wg.Wait()
	return nil
}
