// Package guardcoldstart is the Guard H rehearsal engine: it proves, with a
// hash-chained receipt, that a node recovers one already admitted local
// application from cached artifacts alone after its WAN and control-plane
// access were cut and the host rebooted.
//
// It adds no executor. The recovery steps run through the StepRunner the
// caller supplies, which is the pinned StackKits CLI behind the Guard
// executor; the engine only admits the plan, verifies what is cached, confines
// every step's network access, refuses on expired authority and records what
// happened. Standard Mode independence holds because the plan may need no
// kombify account and names no step kind beyond a node-local restart,
// local-state restore and local verification.
package guardcoldstart

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"time"

	"github.com/kombifyio/homelab/pkg/guardbinding"
)

const (
	// PlanContractVersion names the signed cold-start plan.
	PlanContractVersion = "guard.cold-start-plan/v1"

	planSignatureDomain = "homelab.guard.cold-start-plan/v1\x00"

	// MaxPlanArtifacts and MaxPlanSteps bound a plan so verification stays cheap.
	MaxPlanArtifacts = 64
	MaxPlanSteps     = 64
	// MaxArtifactBytes bounds one cached artifact read.
	MaxArtifactBytes int64 = 8 << 30

	// RoleStackKitsCLI is the pinned release the steps run through. Exactly one
	// artifact has this role and its digest is the plan release's archive digest.
	RoleStackKitsCLI = "stackkits-cli"
	// RoleWorkload, RoleRecipe and RoleState are the other cached inputs.
	RoleWorkload = "workload"
	RoleRecipe   = "recipe"
	RoleState    = "state"
)

// Step kinds are the node-local recovery actions. There is deliberately no
// kind for Terramate, provider, multi-server or multi-site work: those stay
// Homelab Runtime-only and are refused as unknown.
const (
	StepRestartWorkload = "restart_workload"
	StepRestoreState    = "restore_local_state"
	StepVerifyLocal     = "verify_local"
)

// Dependency kinds a plan declares as unreachable during the rehearsal.
const (
	DependencyDNS          = "dns"
	DependencyRegistry     = "registry"
	DependencyIdentity     = "identity"
	DependencyControlPlane = "control_plane"
	DependencyWAN          = "wan"
)

var (
	idPattern     = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Artifact is one cached input, named by its digest.
type Artifact struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Step is one ordered node-local recovery action. Artifact optionally names
// the cached artifact the step consumes.
type Step struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Artifact string `json:"artifact,omitempty"`
}

// Dependency is one remote dependency the recipe must not need. Host is a name
// or address; a rehearsal that observes it reachable refuses to start, because
// the cut was not proven.
type Dependency struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Host string `json:"host"`
}

// Plan is the control plane's signed statement of one previously admitted
// local recovery recipe for one node. It is issued online; the node holds it in
// durable local custody across the reboot.
type Plan struct {
	ContractVersion string                       `json:"contract_version"`
	PlanID          string                       `json:"plan_id"`
	TenantID        string                       `json:"tenant_id"`
	WorkerID        string                       `json:"worker_id"`
	ServerID        string                       `json:"server_id,omitempty"`
	Generation      int64                        `json:"generation"`
	Release         guardbinding.LocalReleasePin `json:"release"`
	Artifacts       []Artifact                   `json:"artifacts"`
	Steps           []Step                       `json:"steps"`
	Dependencies    []Dependency                 `json:"dependencies"`
	// LocalPrefixes are the only networks a step may reach (loopback and the
	// node's own LAN). Hostnames are never reachable: there is no remote DNS.
	LocalPrefixes []string `json:"local_prefixes"`
	// RequiresAccount must be false: Standard Mode needs no kombify account.
	RequiresAccount bool      `json:"requires_account"`
	IssuedAt        time.Time `json:"issued_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	KeyID           string    `json:"key_id,omitempty"`
	Signature       string    `json:"signature,omitempty"`
}

// AuthorityUntil is the end of the plan's offline authority: the earlier of its
// own expiry and the G07 offline lifetime after issue. The offline authority
// is never renewed locally.
func (p Plan) AuthorityUntil() time.Time {
	until := p.IssuedAt.Add(guardbinding.LocalOfflineLifetime)
	if p.ExpiresAt.Before(until) {
		return p.ExpiresAt
	}
	return until
}

// SigningMessage is the domain-separated canonical message the control plane
// signs with its pinned Ed25519 key.
func (p Plan) SigningMessage() ([]byte, error) {
	p.Signature = ""
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	canonical, err := guardbinding.Canonicalize(raw)
	if err != nil {
		return nil, err
	}
	return append([]byte(planSignatureDomain), canonical...), nil
}

// Digest is the SHA-256 of the signing message, the plan's identity in a receipt.
func (p Plan) Digest() (string, error) {
	message, err := p.SigningMessage()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(message)
	return hex.EncodeToString(sum[:]), nil
}

// VerifySignature checks the plan against the pinned keys. A plan is never
// admitted unsigned and without a pinned key a node holds no cold-start
// authority.
func (p Plan) VerifySignature(pins []guardbinding.PinnedKey) *guardbinding.Denial {
	if len(pins) == 0 {
		return guardbinding.Denyf(guardbinding.CodeLocalAuthorityMissing, "this Guard pins no control-plane signing key, so it holds no cold-start authority")
	}
	if p.Signature == "" {
		return guardbinding.Denyf(CodePlanInvalid, "the cold-start plan is unsigned")
	}
	var public ed25519.PublicKey
	for _, pin := range pins {
		if pin.KeyID != p.KeyID {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(pin.PublicKey)
		if err != nil || len(raw) != ed25519.PublicKeySize || guardbinding.KeyIDOf(raw) != pin.KeyID {
			return guardbinding.Denyf(CodePlanInvalid, "pinned key %q is malformed", pin.KeyID)
		}
		public = raw
	}
	if public == nil {
		return guardbinding.Denyf(CodePlanInvalid, "the cold-start plan is signed by key %q, which this Guard does not pin", p.KeyID)
	}
	signature, err := base64.RawURLEncoding.DecodeString(p.Signature)
	if err != nil {
		return guardbinding.Denyf(CodePlanInvalid, "the cold-start plan signature is malformed")
	}
	message, err := p.SigningMessage()
	if err != nil || !ed25519.Verify(public, message, signature) {
		return guardbinding.Denyf(CodePlanInvalid, "the cold-start plan signature does not verify")
	}
	return nil
}

// Validate is the structural check of a plan whose signature already verified.
func (p Plan) Validate() *guardbinding.Denial {
	bad := func(format string, args ...any) *guardbinding.Denial {
		return guardbinding.Denyf(CodePlanInvalid, format, args...)
	}
	switch {
	case p.ContractVersion != PlanContractVersion:
		return guardbinding.Denyf(guardbinding.CodeContractUnsupported, "cold-start contract %q is not supported", p.ContractVersion)
	case !idPattern.MatchString(p.PlanID) || p.TenantID == "" || p.WorkerID == "" || p.Generation < 0:
		return bad("the cold-start plan names no complete binding")
	case p.RequiresAccount:
		return bad("the cold-start plan requires a kombify account, which Standard Mode never does")
	case p.IssuedAt.IsZero() || p.ExpiresAt.IsZero() || !p.IssuedAt.Before(p.ExpiresAt):
		return guardbinding.Denyf(guardbinding.CodeAuthorityExpired, "the cold-start plan has no lifetime left")
	case len(p.Artifacts) == 0 || len(p.Artifacts) > MaxPlanArtifacts || len(p.Steps) == 0 || len(p.Steps) > MaxPlanSteps:
		return bad("the cold-start plan has no bounded artifact and step list")
	case len(p.Dependencies) == 0:
		return bad("the cold-start plan declares no remote dependency to cut")
	case len(p.LocalPrefixes) == 0:
		return bad("the cold-start plan names no local network")
	}
	for _, prefix := range p.LocalPrefixes {
		if _, err := netip.ParsePrefix(prefix); err != nil {
			return bad("local prefix %q is not a CIDR", prefix)
		}
	}
	byName := map[string]Artifact{}
	cli := 0
	for _, a := range p.Artifacts {
		if !idPattern.MatchString(a.Name) || !sha256Pattern.MatchString(a.SHA256) || a.Size <= 0 || a.Size > MaxArtifactBytes {
			return bad("artifact %q is not fully named by name, digest and size", a.Name)
		}
		switch a.Role {
		case RoleStackKitsCLI:
			cli++
			if a.SHA256 != p.Release.ArchiveSHA256 {
				return bad("the %s artifact is not the release the plan pins", RoleStackKitsCLI)
			}
		case RoleWorkload, RoleRecipe, RoleState:
		default:
			return bad("artifact %q has unknown role %q", a.Name, a.Role)
		}
		if _, dup := byName[a.Name]; dup {
			return bad("artifact %q is listed twice", a.Name)
		}
		byName[a.Name] = a
	}
	if cli != 1 || p.Release.Version == "" || !sha256Pattern.MatchString(p.Release.ArchiveSHA256) {
		return bad("the plan needs exactly one pinned %s artifact", RoleStackKitsCLI)
	}
	seen := map[string]bool{}
	for _, s := range p.Steps {
		if !idPattern.MatchString(s.ID) || seen[s.ID] {
			return bad("step id %q is missing or repeated", s.ID)
		}
		seen[s.ID] = true
		switch s.Kind {
		case StepRestartWorkload, StepRestoreState, StepVerifyLocal:
		default:
			return bad("step %q has kind %q, which is not a node-local recovery action", s.ID, s.Kind)
		}
		if s.Artifact != "" {
			if _, ok := byName[s.Artifact]; !ok {
				return bad("step %q names artifact %q, which the plan does not list", s.ID, s.Artifact)
			}
		}
	}
	for _, d := range p.Dependencies {
		switch d.Kind {
		case DependencyDNS, DependencyRegistry, DependencyIdentity, DependencyControlPlane, DependencyWAN:
		default:
			return bad("dependency %q has unknown kind %q", d.Name, d.Kind)
		}
		if d.Name == "" || d.Host == "" {
			return bad("a dependency needs a name and host")
		}
	}
	return nil
}

// EffectID is the stable identity of one step's effect under this plan, the
// key by which an effect is never repeated and reconciled on reconnection.
func (p Plan) EffectID(stepID string) string {
	sum := sha256.Sum256([]byte("homelab.guard.cold-start-effect/v1\x00" + p.PlanID + "\x00" + stepID))
	return fmt.Sprintf("cse-%s", hex.EncodeToString(sum[:12]))
}
