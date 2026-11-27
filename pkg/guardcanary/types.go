package guardcanary

import (
	"context"
	"time"
)

// Schema identifies the receipt format and PackageVersion the producing logic.
const (
	ReceiptSchema  = "stackkits.guard-canary-qualification/v1"
	PackageVersion = "guardcanary/v1"
	// SliceStatelessOCI is the only supported slice: one stateless OCI app on one
	// Linux host. Stateful workloads need a separate compatibility contract.
	SliceStatelessOCI = "single-stateless-oci-app/linux"
)

// Release is one deployable state: an exact image digest reference and the
// configuration it runs with. Reference must be repository@sha256:<64 hex>;
// mutable tags are not a release.
type Release struct {
	Reference    string `json:"reference"`
	Digest       string `json:"digest"`
	ConfigSHA256 string `json:"configSha256"`
}

// SignatureEvidence is what an injected [Verifier] observed for a reference.
type SignatureEvidence struct {
	// Verified reports a successful cryptographic verification of a signature.
	Verified bool
	// Digest is the image digest the verified signature covers.
	Digest string
	// Issuer and Subject are the verified signing identity (for keyless
	// signatures the OIDC issuer and the certificate subject).
	Issuer  string
	Subject string
	// ObservedAt is when the verifier observed the signature.
	ObservedAt time.Time
}

// Verifier verifies the signature of an exact image reference. An error means
// nothing was verified.
type Verifier interface {
	Verify(ctx context.Context, reference string) (SignatureEvidence, error)
}

// Running is the observed runtime state of the application.
type Running struct {
	// Digests lists every image digest currently running for the application,
	// so a leftover instance of a rejected digest is visible.
	Digests      []string
	ConfigSHA256 string
	ObservedAt   time.Time
}

// Sample is one probe of the running application.
type Sample struct {
	ObservedAt time.Time
	// Digest is the image digest of the instance that answered the probe.
	Digest string
	// Started and Ready are the startup and readiness probe results.
	Started bool
	Ready   bool
	// StartupDuration is how long the instance took to start.
	StartupDuration time.Duration
	// Requests and Errors count requests served and failed since the previous
	// sample; LatencyP95 is the 95th percentile over those requests.
	Requests   uint64
	Errors     uint64
	LatencyP95 time.Duration
}

// Runtime applies releases and observes the result. Apply must be deterministic
// for a Release: the same digest and configuration, nothing else.
type Runtime interface {
	Running(ctx context.Context) (Running, error)
	Apply(ctx context.Context, release Release) error
	Probe(ctx context.Context) (Sample, error)
}

// Signer is one allow-listed signing identity. Both fields match exactly.
type Signer struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// Policy bounds admission and the canary. Every field is required; the zero
// value is refused so nothing runs under an implicit default.
type Policy struct {
	TrustedSigners []Signer
	// MaxEvidenceAge is the oldest signature/runtime observation accepted.
	MaxEvidenceAge time.Duration
	// MaxClockSkew is how far an observation may lie in the future.
	MaxClockSkew time.Duration

	// Window bounds the whole canary; SampleInterval spaces probes.
	Window         time.Duration
	SampleInterval time.Duration
	// MinSamples fresh samples are required; MinRequests requests in total are
	// required before an error rate counts as evidence.
	MinSamples  int
	MinRequests uint64
	// MaxSampleAge is the oldest probe sample accepted when assessing.
	MaxSampleAge time.Duration
	// StartupDeadline is the longest accepted startup.
	StartupDeadline time.Duration
	// MaxErrorRate is the highest accepted errors/requests ratio in [0,1).
	MaxErrorRate float64
	// MaxLatencyP95 is the highest accepted 95th percentile latency.
	MaxLatencyP95 time.Duration
	// RollbackTimeout bounds the rollback and its recovery assessment, which run
	// on a context detached from a cancelled caller.
	RollbackTimeout time.Duration
}

// Request describes one update. Stateless must be declared true: a stateful
// application is refused, never migrated or rolled back here.
type Request struct {
	App       string
	Stateless bool
	Previous  Release
	Candidate Release
	// EvidenceSource labels where the ports got their observations from, for
	// example "live-host" or "scenario-fake". It is recorded, never trusted.
	EvidenceSource string
}

// Config wires the ports. Now and Sleep default to the real clock.
type Config struct {
	Policy   Policy
	Verifier Verifier
	Runtime  Runtime
	Now      func() time.Time
	Sleep    func(ctx context.Context, d time.Duration) error
}
