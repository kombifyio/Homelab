package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"

	"github.com/kombifyio/homelab/internal/ownapplication"
	"github.com/kombifyio/homelab/internal/stackkitrelease"
)

func runOwnApplicationDraft(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("own-app", flag.ContinueOnError)
	document := flags.String("document", "", "Versioned desired document to admit into Homelab Runtime")
	workspace := flags.String("workspace", ".", "Installation workspace containing Homelab Runtime's desired state")
	installation := flags.String("installation-file", "", "Offline routing context for inspection only")
	dry := flags.Bool("dry-run", false, "Preview without replacing the desired document")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *document == "" {
		return errors.New("usage: homelab own-app --document <JSON> [--workspace <directory>] [--dry-run]")
	}
	result, err := ownapplication.Draft(ctx, stackkitrelease.Cache{Root: os.Getenv("HOMELAB_STACKKIT_RELEASE_CACHE")}, os.Getenv("HOMELAB_STACKKIT_RELEASE_PIN"), ownapplication.DraftRequest{Document: *document, Workspace: *workspace, InstallationFile: *installation, DryRun: *dry})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
