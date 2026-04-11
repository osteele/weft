package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/r2"
	"github.com/spf13/cobra"
)

const r2CmdTimeout = 15 * time.Second

func init() {
	rootCmd.AddCommand(r2Cmd)
	r2Cmd.AddCommand(r2StatusCmd)
	r2Cmd.AddCommand(r2LsCmd)
	r2Cmd.AddCommand(r2CatCmd)
}

var r2Cmd = &cobra.Command{
	Use:   "r2",
	Short: "Low-level R2 storage operations",
}

func newR2Client() (*r2.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return r2.New(r2Config(cfg))
}

var r2StatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Check whether R2 storage is reachable",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newR2Client()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		_, err = client.ListObjects(ctx, "instance/0/") // lightweight probe
		elapsed := time.Since(start)
		if err != nil {
			fmt.Printf("unreachable (%s): %v\n", elapsed.Round(time.Millisecond), err)
			os.Exit(1)
		}
		fmt.Printf("ok (%s)\n", elapsed.Round(time.Millisecond))
		return nil
	},
}

var r2LsCmd = &cobra.Command{
	Use:   "ls <prefix>",
	Short: "List objects under a prefix",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newR2Client()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), r2CmdTimeout)
		defer cancel()
		objects, err := client.ListObjects(ctx, args[0])
		if err != nil {
			return err
		}
		for _, obj := range objects {
			fmt.Printf("%10d  %s\n", obj.SizeBytes, obj.Key)
		}
		return nil
	},
}

var r2CatCmd = &cobra.Command{
	Use:   "cat <key>",
	Short: "Print the contents of an object",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := newR2Client()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), r2CmdTimeout)
		defer cancel()
		data, err := client.GetObject(ctx, args[0])
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(data)
		return err
	},
}
