package cmd

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/cloudreconcile"
	"github.com/osteele/weft/internal/cloudsync"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/monitor"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/web"
	"github.com/spf13/cobra"
)

var webCmd = &cobra.Command{
	Use:   "web",
	Short: "Read-only web UI for monitoring jobs",
	RunE:  runWeb,
}

var webPort int
var webOpen bool

func init() {
	rootCmd.AddCommand(webCmd)
	webCmd.Flags().IntVar(&webPort, "port", 0, "Port to serve the web UI on (localhost only)")
	webCmd.Flags().BoolVar(&webOpen, "open", false, "Open the web UI in a browser")
}

func runWeb(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	monCfg := monitor.DefaultConfig()
	if cfg.SyncActiveInterval > 0 {
		monCfg.SyncActiveInterval = time.Duration(cfg.SyncActiveInterval) * time.Second
	} else if cfg.SyncInterval > 0 {
		monCfg.SyncActiveInterval = time.Duration(cfg.SyncInterval) * time.Second
	}
	if cfg.SyncIdleInterval > 0 {
		monCfg.SyncIdleInterval = time.Duration(cfg.SyncIdleInterval) * time.Second
	}
	if cfg.HostRefreshInterval > 0 {
		monCfg.HostRefreshInterval = time.Duration(cfg.HostRefreshInterval) * time.Second
	}

	mon := monitor.New(database, monCfg)
	mon.EnableRemediation(cfg)
	mon.Start()
	defer mon.Stop()
	stopCloudReconcileLoop := startWebCloudReconcileLoop(database, cfg)
	defer stopCloudReconcileLoop()

	port := cfg.WebPort
	if webPort != 0 {
		port = webPort
	}

	server, err := web.NewServer(mon, web.Config{Port: port})
	if err != nil {
		return err
	}

	url, err := server.Start()
	if err != nil {
		return err
	}

	fmt.Printf("Web UI running at %s\n", url)
	if webOpen {
		if err := openURL(url); err != nil {
			return err
		}
	}

	quit := listenForQuit()
	sigch := make(chan os.Signal, 1)
	signal.Notify(sigch, os.Interrupt, syscall.SIGTERM)

	select {
	case <-quit:
	case <-sigch:
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Stop(ctx)
}

func startWebCloudReconcileLoop(database *sql.DB, cfg *config.Config) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	clients, _ := buildCloudClients(cfg)
	r2Client, _ := buildR2Client(cfg)
	reconciler := campaign.NewReconciler()
	owner := cloudreconcile.OwnerID()

	go func() {
		defer close(done)
		runPass := func() {
			cloudreconcile.RunTwoPhasePass(ctx, database, cloudreconcile.Config{
				Owner: owner,
			}, func(_ context.Context, _ bool) (int, error) {
				return runWebCloudReconcilePhase(database, clients, r2Client, reconciler)
			})
		}

		runPass()
		ticker := time.NewTicker(cloudreconcile.DefaultInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runPass()
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

func runWebCloudReconcilePhase(database *sql.DB, clients []cloud.Client, r2Client *r2.Client, reconciler *campaign.Reconciler) (int, error) {
	if len(clients) == 0 {
		resetMap, err := db.ResetJobsOnTerminalLaunches(database)
		if err != nil {
			return 0, err
		}
		return len(resetMap), nil
	}
	result := cloudsync.SyncState(database, reconciler, clients, r2Client, nil)
	if result.ReconcileResult == nil {
		return 0, nil
	}
	return result.ReconcileResult.Reconciled, nil
}

func openURL(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "linux":
		return exec.Command("xdg-open", url).Start()
	default:
		return nil
	}
}

func listenForQuit() <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fd := os.Stdin.Fd()
		if !term.IsTerminal(fd) {
			reader := bufio.NewReader(os.Stdin)
			for {
				r, _, err := reader.ReadRune()
				if err != nil {
					return
				}
				if r == 'q' || r == 'Q' {
					return
				}
			}
		}

		oldState, err := term.MakeRaw(fd)
		if err != nil {
			return
		}
		defer term.Restore(fd, oldState)

		cont := make(chan os.Signal, 1)
		signal.Notify(cont, syscall.SIGCONT)
		defer signal.Stop(cont)

		reader := bufio.NewReader(os.Stdin)
		for {
			b, err := reader.ReadByte()
			if err != nil {
				return
			}
			switch b {
			case 'q', 'Q':
				return
			case 0x1a: // Ctrl-Z
				_ = term.Restore(fd, oldState)
				_ = syscall.Kill(syscall.Getpid(), syscall.SIGTSTP)
				<-cont
				oldState, err = term.MakeRaw(fd)
				if err != nil {
					return
				}
			}
		}
	}()
	return done
}
