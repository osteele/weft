package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/runner"
)

// runR2Command handles `weft-agent r2 <content-info|put-content>`. These mirror
// the full CLI's `weft r2 ...` subcommands so the host-side checkpoint staging
// in internal/dataloc/r2put.go can invoke the binary that is actually deployed
// to on-prem hosts (the agent build), which otherwise has no `r2` CLI surface.
func runR2Command(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "r2: expected content-info or put-content")
		os.Exit(2)
	}
	switch args[0] {
	case "content-info":
		runR2ContentInfo(args[1:])
	case "put-content":
		runR2PutContent(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "r2: unknown subcommand %q\n", args[0])
		os.Exit(2)
	}
}

func runR2ContentInfo(args []string) {
	fs := flag.NewFlagSet("r2 content-info", flag.ExitOnError)
	path := fs.String("path", "", "Local path to digest")
	_ = fs.Parse(args)
	if *path == "" {
		fmt.Fprintln(os.Stderr, "r2 content-info: --path is required")
		os.Exit(2)
	}
	info, err := dataloc.DigestPath(runner.ExpandTilde(*path))
	if err != nil {
		fmt.Fprintf(os.Stderr, "r2 content-info: %v\n", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(info); err != nil {
		fmt.Fprintf(os.Stderr, "r2 content-info: %v\n", err)
		os.Exit(1)
	}
}

func runR2PutContent(args []string) {
	fs := flag.NewFlagSet("r2 put-content", flag.ExitOnError)
	path := fs.String("path", "", "Local path to upload")
	key := fs.String("key", "", "R2 object key")
	contentTypeStr := fs.String("content-type", string(dataloc.ContentTypeFile), "Content type: file or directory")
	credsStdin := fs.Bool("creds-stdin", false, "Read R2 credentials as JSON from stdin")
	_ = fs.Parse(args)
	if *path == "" || *key == "" {
		fmt.Fprintln(os.Stderr, "r2 put-content: --path and --key are required")
		os.Exit(2)
	}
	contentType := dataloc.ContentType(*contentTypeStr)
	switch contentType {
	case dataloc.ContentTypeFile, dataloc.ContentTypeDirectory:
	default:
		fmt.Fprintf(os.Stderr, "r2 put-content: --content-type must be %q or %q\n",
			dataloc.ContentTypeFile, dataloc.ContentTypeDirectory)
		os.Exit(2)
	}
	if !*credsStdin {
		// The agent has no R2 config of its own; credentials always arrive on
		// stdin from the launcher (never in argv/env, which are ps-visible).
		fmt.Fprintln(os.Stderr, "r2 put-content: --creds-stdin is required")
		os.Exit(2)
	}
	var creds dataloc.RemoteR2Credentials
	if err := json.NewDecoder(os.Stdin).Decode(&creds); err != nil {
		fmt.Fprintf(os.Stderr, "r2 put-content: decode credentials from stdin: %v\n", err)
		os.Exit(1)
	}
	client, err := dataloc.R2ClientFromCredentials(creds)
	if err != nil {
		fmt.Fprintf(os.Stderr, "r2 put-content: %v\n", err)
		os.Exit(1)
	}
	if err := dataloc.PutContentToR2(context.Background(), client, runner.ExpandTilde(*path), contentType, *key); err != nil {
		fmt.Fprintf(os.Stderr, "r2 put-content: %v\n", err)
		os.Exit(1)
	}
}
