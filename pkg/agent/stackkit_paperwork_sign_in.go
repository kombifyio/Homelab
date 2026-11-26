package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/kombifyio/homelab/pkg/guardreach"
)

var paperworkBundleDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var paperworkBundleSource = regexp.MustCompile(`^[0-9a-f]{40}$`)

type PaperworkBundlePin struct {
	SourceSHA    string `json:"source_sha"`
	Image        string `json:"image"`
	BundleSHA256 string `json:"bundle_sha256"`
	BundleSize   int64  `json:"bundle_size"`
}

func (pin PaperworkBundlePin) Ready() bool {
	repository, digest, pinned := strings.Cut(pin.Image, "@")
	return paperworkBundleSource.MatchString(pin.SourceSHA) && paperworkBundleDigest.MatchString(pin.BundleSHA256) &&
		pinned && strings.HasPrefix(repository, "api.kombify.io/") && paperworkBundleDigest.MatchString(digest) &&
		pin.BundleSize > 0 && pin.BundleSize <= 2<<20
}

type PaperworkSignInCustody struct {
	Executor  *StackKitExecutor
	Workspace string
	Bundle    PaperworkBundlePin
}

func (custody PaperworkSignInCustody) PaperworkRecipient(ctx context.Context, challenge string) (json.RawMessage, error) {
	return custody.run(ctx, 15*time.Second, "", nil, "recipient", "--challenge", challenge, "--owner-approve")
}

func (custody PaperworkSignInCustody) ApplyPaperworkSignIn(ctx context.Context, task guardreach.PaperworkSignInTask) error {
	binding := []string{"--owner-approve", "--tenant-ref", task.TenantID, "--server-id", task.ServerID,
		"--guard-generation", strconv.FormatInt(task.Generation, 10)}
	result, err := custody.run(ctx, 15*time.Second, task.Envelope, nil, append([]string{"apply-paperwork-sign-in"}, binding...)...)
	if err != nil {
		return err
	}
	var reply struct {
		State string `json:"state"`
	}
	if json.Unmarshal(result, &reply) != nil || reply.State != "stored" {
		return errors.New("paperwork custody did not confirm the imported delivery")
	}
	args := append([]string{"install-paperwork-bundle", "--source-sha", custody.Bundle.SourceSHA,
		"--image", custody.Bundle.Image, "--bundle-sha256", custody.Bundle.BundleSHA256,
		"--bundle-size", strconv.FormatInt(custody.Bundle.BundleSize, 10)}, binding...)
	result, err = custody.run(ctx, 5*time.Minute, "", []string{"PAPERWORK_AUTH_MODE=linked", "PAPERWORK_PUBLIC_URL=" + task.Origin}, args...)
	if err != nil {
		return err
	}
	var installed struct {
		State        string `json:"state"`
		SourceSHA    string `json:"source_sha"`
		Image        string `json:"image"`
		BundleSHA256 string `json:"bundle_sha256"`
	}
	if json.Unmarshal(result, &installed) != nil || installed.State != "compose_ready" || installed.SourceSHA != custody.Bundle.SourceSHA ||
		installed.Image != custody.Bundle.Image || installed.BundleSHA256 != custody.Bundle.BundleSHA256 {
		return errors.New("paperwork installation did not confirm the admitted bundle")
	}
	return nil
}

func (custody PaperworkSignInCustody) run(ctx context.Context, timeout time.Duration, input string, environment []string, command ...string) (json.RawMessage, error) {
	if runtime.GOOS != "linux" || os.Getenv("STACKKIT_LINKED_PAPERWORK_INSTALL") != "true" || !custody.Executor.Available() || !custody.Bundle.Ready() {
		return nil, errors.New("paperwork custody is disabled on this node")
	}
	workspace, err := confineStackKitWorkDir(custody.Workspace)
	if err != nil {
		return nil, errors.New("paperwork workspace is unavailable")
	}
	release, err := custody.Executor.releaseCache().ResolvePin(custody.Executor.pinPath)
	if err != nil {
		return nil, errors.New("pinned StackKits release is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := append([]string{"--no-log", "--chdir", workspace, "host", "delivery"}, command...)
	process := newStackKitProcess(ctx, release.BinaryPath(), args...)
	process.Dir = workspace
	for _, entry := range stackKitChildEnvironment(os.Environ()) {
		if !strings.HasPrefix(entry, "PAPERWORK_") {
			process.Env = append(process.Env, entry)
		}
	}
	process.Env = append(process.Env, environment...)
	process.Stdin = strings.NewReader(input)
	stdout := newBoundedStackKitBuffer(16 << 10)
	process.Stdout = stdout
	if err := process.Run(); err != nil {
		return nil, errors.New("pinned StackKits Paperwork custody command failed")
	}
	var reply struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if json.Unmarshal([]byte(stdout.String()), &reply) != nil || reply.Status != "success" || len(reply.Data) < 2 || reply.Data[0] != '{' {
		return nil, errors.New("pinned StackKits Paperwork custody response was invalid")
	}
	return reply.Data, nil
}
