package stackkitcommand

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"

	"github.com/kombifyio/homelab/pkg/api/agentpb"
)

// One Authority across servers (homelab identity target §4, P2). Homelab
// carries the join of a further server to the homelab's Authority as closed
// hand-off steps through the pinned StackKits CLI: the join request on the
// joining server, the Owner's approval at the Authority (the Owner's passkey
// step-up in the Authority's Hub is the gate, Homelab cannot answer it),
// the install of the Authority-signed trust bundle, the registration of the
// joined server's sign-in client, its binding after Wallet host delivery and
// the homelab DNS zone. Every document is public or sealed; Homelab signs
// none of them.
const (
	// IdentityJoinCapability marks an agent whose StackKits CLI runs the
	// identity join steps.
	IdentityJoinCapability = "stackkit.identity-join.v1"
	// MinIdentityJoinRelease is the first StackKits release with the joined
	// core and the identity join steps.
	MinIdentityJoinRelease = "v0.56.5"

	HandoffIdentityJoinRequest    = "identity-join-request"
	HandoffIdentityJoinApprove    = "identity-join-approve"
	HandoffIdentityJoinInstall    = "identity-join-install"
	HandoffIdentityClientRegister = "identity-client-register"
	HandoffIdentityClientBind     = "identity-client-bind"
	HandoffIdentityZoneSite       = "identity-zone-site"
	HandoffIdentityZoneInstall    = "identity-zone-install"

	// IdentityJoinRequestInputSchema is the input of identity-join-request:
	// the root fingerprint the Owner read at the Authority and the hostnames.
	IdentityJoinRequestInputSchema = "homelab.identity-join-request-input/v1"
)

var (
	identityJoinRootPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	identityJoinHostnamePattern = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	identityJoinSchemas         = map[string]string{
		HandoffIdentityJoinApprove:    "stackkit.homelab-join-request/v1",
		HandoffIdentityJoinInstall:    "stackkit.homelab-trust-bundle/v1",
		HandoffIdentityClientRegister: "stackkit.homelab-oidc-client-request/v1",
		HandoffIdentityClientBind:     "stackkit.homelab-oidc-client-binding/v1",
		HandoffIdentityZoneInstall:    "stackkit.homelab-dns-zone/v1",
	}
)

func init() {
	handoffSteps = append(handoffSteps,
		HandoffStep{Name: HandoffIdentityJoinRequest, NeedsDocument: true, ProducesDocument: true},
		HandoffStep{Name: HandoffIdentityJoinApprove, Authority: true, NeedsDocument: true, ProducesDocument: true},
		HandoffStep{Name: HandoffIdentityJoinInstall, NeedsDocument: true, ProducesDocument: true},
		HandoffStep{Name: HandoffIdentityClientRegister, Authority: true, NeedsDocument: true, ProducesDocument: true},
		HandoffStep{Name: HandoffIdentityClientBind, NeedsDocument: true},
		HandoffStep{Name: HandoffIdentityZoneSite, ProducesDocument: true},
		HandoffStep{Name: HandoffIdentityZoneInstall, NeedsDocument: true, ProducesDocument: true},
	)
}

// IsIdentityJoinStep reports a hand-off step of the identity join.
func IsIdentityJoinStep(step string) bool {
	switch step {
	case HandoffIdentityJoinRequest, HandoffIdentityJoinApprove, HandoffIdentityJoinInstall, HandoffIdentityClientRegister,
		HandoffIdentityClientBind, HandoffIdentityZoneSite, HandoffIdentityZoneInstall:
		return true
	}
	return false
}

// IdentityJoinRequestInput is the identity-join-request document.
type IdentityJoinRequestInput struct {
	Schema        string   `json:"schema"`
	AuthorityRoot string   `json:"authorityRoot"`
	Hostnames     []string `json:"hostnames"`
}

// ParseIdentityJoinRequestInput decodes the request input strictly: a
// 64-hex root fingerprint and 1 to 32 sorted, unique lowercase hostnames.
func ParseIdentityJoinRequestInput(raw []byte) (IdentityJoinRequestInput, error) {
	var input IdentityJoinRequestInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(new(any)) != io.EOF {
		return input, fmt.Errorf("identity join request input is not one strict JSON document")
	}
	if input.Schema != IdentityJoinRequestInputSchema || !identityJoinRootPattern.MatchString(input.AuthorityRoot) ||
		len(input.Hostnames) == 0 || len(input.Hostnames) > 32 || !slices.IsSorted(input.Hostnames) {
		return input, fmt.Errorf("identity join request input needs the Authority root fingerprint and 1 to 32 sorted hostnames")
	}
	for index, host := range input.Hostnames {
		if len(host) > 253 || !identityJoinHostnamePattern.MatchString(host) || (index > 0 && input.Hostnames[index-1] == host) {
			return input, fmt.Errorf("identity join request input names an invalid or repeated hostname")
		}
	}
	return input, nil
}

// validateIdentityJoinDocument checks the document of an identity join step
// before dispatch: the request input strictly, every other document by its
// schema. StackKits verifies each document's content and signatures itself.
func validateIdentityJoinDocument(step string, document []byte) error {
	if step == HandoffIdentityJoinRequest {
		_, err := ParseIdentityJoinRequestInput(document)
		return err
	}
	want, ok := identityJoinSchemas[step]
	if !ok {
		return nil
	}
	var head struct {
		Schema string `json:"schema"`
	}
	if json.Unmarshal(document, &head) != nil || head.Schema != want {
		return fmt.Errorf("StackKit identity join step %s needs a %s document", step, want)
	}
	return nil
}

// IdentityJoinArgs renders one identity join step as the pinned CLI argv.
// file is the workspace-relative input document, out the workspace-relative
// file the step writes its document to (empty when it prints it).
func IdentityJoinArgs(command *agentpb.StackKitCommand, file, out string) ([]string, error) {
	switch command.HandoffStep {
	case HandoffIdentityJoinRequest:
		input, err := ParseIdentityJoinRequestInput(command.HandoffDocument)
		if err != nil {
			return nil, err
		}
		args := []string{"identity", "join", "request", "--authority-root", input.AuthorityRoot}
		for _, host := range input.Hostnames {
			args = append(args, "--hostname", host)
		}
		return append(args, "--out", out), nil
	case HandoffIdentityJoinApprove:
		return []string{"identity", "join", "approve", "--request", file, "--out", out}, nil
	case HandoffIdentityJoinInstall:
		return []string{"identity", "join", "install", "--bundle", file}, nil
	case HandoffIdentityClientRegister:
		return []string{"identity", "join", "client", "register", "--request", file, "--out", out}, nil
	case HandoffIdentityClientBind:
		return []string{"identity", "join", "client", "bind", "--binding", file}, nil
	case HandoffIdentityZoneSite:
		return []string{"identity", "join", "zone", "site"}, nil
	case HandoffIdentityZoneInstall:
		return []string{"identity", "join", "zone", "install", "--zone", file}, nil
	}
	return nil, fmt.Errorf("StackKit identity join step %q has no command", command.HandoffStep)
}

// IdentityJoinWritesFile reports a step whose CLI writes its document to
// --out instead of printing it.
func IdentityJoinWritesFile(step string) bool {
	switch step {
	case HandoffIdentityJoinRequest, HandoffIdentityJoinApprove, HandoffIdentityClientRegister:
		return true
	}
	return false
}
