// Package ownapplication owns desired documents. StackKits alone validates,
// renders and executes their closed, versioned public contract.
package ownapplication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"github.com/kombifyio/homelab/internal/stackkitrelease"
)

type DraftRequest struct {
	Document         string
	Workspace        string
	InstallationFile string
	DryRun           bool
}

type DraftResult struct {
	DesiredPath string          `json:"desiredPath"`
	Preview     json.RawMessage `json:"preview"`
}

// Draft validates through the authenticated pinned CLI before atomically
// replacing Homelab's desired document. This is authoring, never a rollout;
// managed execution still requires Homelab's Advanced dispatch authority.
func Draft(ctx context.Context, cache stackkitrelease.Cache, pin string, req DraftRequest) (DraftResult, error) {
	var result DraftResult
	release, err := cache.ResolvePin(pin)
	if err != nil {
		return result, err
	}
	raw, err := readDocument(req.Document)
	if err != nil {
		return result, err
	}
	var identity struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &identity) != nil || !regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`).MatchString(identity.Name) || len(identity.Name) > 40 {
		return result, errors.New("own application document needs a bounded name")
	}
	root, err := os.OpenRoot(req.Workspace)
	if err != nil {
		return result, err
	}
	defer root.Close()
	// Stage immutable bytes rather than handing the validator a caller's file
	// that could be replaced between validation and persistence.
	dir := ".homelab/own-applications"
	if err = root.MkdirAll(dir, 0700); err != nil {
		return result, err
	}
	staged, err := root.OpenFile(dir+"/.draft-"+identity.Name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, err
	}
	stageName := dir + "/.draft-" + identity.Name
	defer root.Remove(stageName)
	if _, err = staged.Write(raw); err != nil {
		staged.Close()
		return result, err
	}
	if err = staged.Close(); err != nil {
		return result, err
	}
	abs, err := filepath.Abs(req.Workspace)
	if err != nil {
		return result, err
	}
	args := []string{"-C", abs, "app", "own", "--action", "render", "--name", identity.Name, "--document", filepath.Join(abs, stageName), "--dry-run", "--json"}
	if req.InstallationFile != "" {
		args = append(args, "--installation-file", req.InstallationFile)
	}
	command := exec.CommandContext(ctx, release.BinaryPath(), args...)
	command.Env = []string{"LANG=C", "LC_ALL=C"}
	var stdout bytes.Buffer
	command.Stdout = &boundedWriter{w: &stdout, remaining: 256 << 10}
	// Failure diagnostics can include input from a future CLI; never echo
	// customer environment material into authoring errors or receipts.
	command.Stderr = io.Discard
	if err = command.Run(); err != nil {
		return result, errors.New("pinned StackKits refused the own application document; inspect its version and contract")
	}
	var preview struct {
		APIVersion string `json:"apiVersion"`
	}
	if json.Unmarshal(stdout.Bytes(), &preview) != nil || preview.APIVersion != "stackkit.own-application-result/v1" {
		return result, errors.New("pinned StackKits does not support the own application result contract")
	}
	result.Preview = append(json.RawMessage(nil), stdout.Bytes()...)
	result.DesiredPath = filepath.Join(abs, dir, identity.Name+".json")
	if !req.DryRun {
		if err = root.Rename(stageName, dir+"/"+identity.Name+".json"); err != nil {
			return result, err
		}
	}
	return result, nil
}

func readDocument(name string) ([]byte, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("desired document must be a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw) > 65536 {
		return nil, errors.New("desired document exceeds 65536 bytes")
	}
	return raw, nil
}

type boundedWriter struct {
	w         io.Writer
	remaining int
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	if len(p) > b.remaining {
		return 0, fmt.Errorf("own application preview exceeds its bound")
	}
	n, e := b.w.Write(p)
	b.remaining -= n
	return n, e
}
