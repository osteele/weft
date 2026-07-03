package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/runner"
	"github.com/spf13/cobra"
)

const r2CmdTimeout = 15 * time.Second

func init() {
	rootCmd.AddCommand(r2Cmd)
	r2Cmd.AddCommand(r2StatusCmd)
	r2Cmd.AddCommand(r2LsCmd)
	r2Cmd.AddCommand(r2CatCmd)
	r2Cmd.AddCommand(r2ContentInfoCmd)
	r2Cmd.AddCommand(r2PutContentCmd)
	r2ContentInfoCmd.Flags().StringVar(&r2ContentInfoPath, "path", "", "Local path to digest")
	r2PutContentCmd.Flags().StringVar(&r2PutContentPath, "path", "", "Local path to upload")
	r2PutContentCmd.Flags().StringVar(&r2PutContentKey, "key", "", "R2 object key")
	r2PutContentCmd.Flags().StringVar(&r2PutContentType, "content-type", string(dataloc.ContentTypeFile), "Content type: file or directory")
	r2PutContentCmd.Flags().BoolVar(&r2PutContentCredsStdin, "creds-stdin", false, "Read R2 credentials as JSON from stdin instead of the environment")
}

var r2Cmd = &cobra.Command{
	Use:   "r2",
	Short: "Low-level R2 storage operations",
}

var (
	r2ContentInfoPath      string
	r2PutContentPath       string
	r2PutContentKey        string
	r2PutContentType       string
	r2PutContentCredsStdin bool
)

// r2ClientFromStdinCreds builds an R2 client from a JSON credentials blob read
// from stdin (see dataloc.RemoteR2Credentials). This is how a host-side
// `weft r2 put-content --creds-stdin` receives credentials without them
// appearing in its argv/environment.
func r2ClientFromStdinCreds(r io.Reader) (*r2.Client, error) {
	var creds dataloc.RemoteR2Credentials
	if err := json.NewDecoder(r).Decode(&creds); err != nil {
		return nil, fmt.Errorf("decode R2 credentials from stdin: %w", err)
	}
	return dataloc.R2ClientFromCredentials(creds)
}

func newR2Client() (*r2.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return r2.New(r2Config(cfg))
}

func newR2ClientFromEnvOrConfig() (*r2.Client, error) {
	if cfg, ok, err := r2ConfigFromEnv(); ok || err != nil {
		if err != nil {
			return nil, err
		}
		return r2.New(cfg)
	}
	return newR2Client()
}

func r2ConfigFromEnv() (r2.Config, bool, error) {
	values := map[string]string{
		dataloc.RemoteR2AccountIDEnv:       os.Getenv(dataloc.RemoteR2AccountIDEnv),
		dataloc.RemoteR2AccessKeyIDEnv:     os.Getenv(dataloc.RemoteR2AccessKeyIDEnv),
		dataloc.RemoteR2SecretAccessKeyEnv: os.Getenv(dataloc.RemoteR2SecretAccessKeyEnv),
		dataloc.RemoteR2BucketEnv:          os.Getenv(dataloc.RemoteR2BucketEnv),
	}
	anySet := false
	var missing []string
	for name, value := range values {
		if strings.TrimSpace(value) != "" {
			anySet = true
			continue
		}
		missing = append(missing, name)
	}
	if !anySet {
		return r2.Config{}, false, nil
	}
	if len(missing) > 0 {
		return r2.Config{}, true, fmt.Errorf("incomplete R2 environment; missing %s", strings.Join(missing, ", "))
	}
	return r2.Config{
		AccountID:       values[dataloc.RemoteR2AccountIDEnv],
		AccessKeyID:     values[dataloc.RemoteR2AccessKeyIDEnv],
		SecretAccessKey: values[dataloc.RemoteR2SecretAccessKeyEnv],
		Bucket:          values[dataloc.RemoteR2BucketEnv],
	}, true, nil
}

var r2ContentInfoCmd = &cobra.Command{
	Use:    "content-info --path <path>",
	Short:  "Digest a local file or directory",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		path := strings.TrimSpace(r2ContentInfoPath)
		if path == "" {
			return fmt.Errorf("--path is required")
		}
		info, err := dataloc.DigestPath(runner.ExpandTilde(path))
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(info)
	},
}

var r2PutContentCmd = &cobra.Command{
	Use:    "put-content --key <key> --path <path>",
	Short:  "Upload a local file or directory to R2",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		path := strings.TrimSpace(r2PutContentPath)
		key := strings.TrimSpace(r2PutContentKey)
		if path == "" {
			return fmt.Errorf("--path is required")
		}
		if key == "" {
			return fmt.Errorf("--key is required")
		}
		contentType := dataloc.ContentType(strings.TrimSpace(r2PutContentType))
		switch contentType {
		case dataloc.ContentTypeFile, dataloc.ContentTypeDirectory:
		default:
			return fmt.Errorf("--content-type must be %q or %q", dataloc.ContentTypeFile, dataloc.ContentTypeDirectory)
		}
		var client *r2.Client
		var err error
		if r2PutContentCredsStdin {
			client, err = r2ClientFromStdinCreds(cmd.InOrStdin())
		} else {
			client, err = newR2ClientFromEnvOrConfig()
		}
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		return putContentPathToR2(ctx, client, runner.ExpandTilde(path), contentType, key)
	},
}

func putContentPathToR2(ctx context.Context, client *r2.Client, path string, contentType dataloc.ContentType, key string) error {
	return dataloc.PutContentToR2(ctx, client, path, contentType, key)
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
