package serviceregistry

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// TargetKind identifies the durable runtime target of a service. A server is
// the only target kind that has a ServerID; provider-native workloads are
// intentionally modeled without a fake server aggregate.
type TargetKind string

const (
	TargetKindServer          TargetKind = "server"
	TargetKindManagedWorkload TargetKind = "managed_workload"
	TargetKindUnknown         TargetKind = "unknown"
)

// Placement is the evidence-backed service placement. Managed workload fields
// are data-model support for a future provider slice; they do not enable
// provider provisioning, SLA claims, backup execution, or cleanup today.
type Placement struct {
	TargetKind            TargetKind
	ProviderID            string
	ManagedTargetRef      string
	ProviderReceiptRef    string
	SLAPolicyRef          string
	BackupPolicyRef       string
	IsolationClass        string
	IngressHostname       string
	ManagedServiceSlotRef string
	EvidenceRef           string
	ObservedAt            *time.Time
}

// NormalizePlacement removes transport-only variation and derives the safe
// default only from an actual ServerID. Missing evidence never becomes local
// or managed.
func NormalizePlacement(serverID string, placement Placement) Placement {
	serverID = strings.TrimSpace(serverID)
	placement.TargetKind = TargetKind(normalizePlacementValue(string(placement.TargetKind)))
	placement.ProviderID = normalizePlacementValue(placement.ProviderID)
	placement.ManagedTargetRef = strings.TrimSpace(placement.ManagedTargetRef)
	placement.ProviderReceiptRef = strings.TrimSpace(placement.ProviderReceiptRef)
	placement.SLAPolicyRef = strings.TrimSpace(placement.SLAPolicyRef)
	placement.BackupPolicyRef = strings.TrimSpace(placement.BackupPolicyRef)
	placement.IsolationClass = normalizePlacementValue(placement.IsolationClass)
	placement.IngressHostname = normalizePlacementValue(placement.IngressHostname)
	placement.ManagedServiceSlotRef = strings.TrimSpace(placement.ManagedServiceSlotRef)
	placement.EvidenceRef = strings.TrimSpace(placement.EvidenceRef)
	if placement.ObservedAt != nil {
		observedAt := placement.ObservedAt.UTC()
		placement.ObservedAt = &observedAt
	}
	if placement.TargetKind == "" {
		if serverID != "" {
			placement.TargetKind = TargetKindServer
		} else {
			placement.TargetKind = TargetKindUnknown
		}
	}
	return placement
}

// ClonePlacement keeps runtime projections immutable to callers.
func ClonePlacement(placement Placement) Placement {
	if placement.ObservedAt != nil {
		observedAt := *placement.ObservedAt
		placement.ObservedAt = &observedAt
	}
	return placement
}

// PlacementIntentPresent distinguishes an omitted patch from an explicit
// placement update. TargetKindUnknown is an explicit fail-closed clear.
func PlacementIntentPresent(placement Placement) bool {
	return strings.TrimSpace(string(placement.TargetKind)) != "" ||
		strings.TrimSpace(placement.ProviderID) != "" ||
		strings.TrimSpace(placement.ManagedTargetRef) != "" ||
		strings.TrimSpace(placement.ProviderReceiptRef) != "" ||
		strings.TrimSpace(placement.SLAPolicyRef) != "" ||
		strings.TrimSpace(placement.BackupPolicyRef) != "" ||
		strings.TrimSpace(placement.IsolationClass) != "" ||
		strings.TrimSpace(placement.IngressHostname) != "" ||
		strings.TrimSpace(placement.ManagedServiceSlotRef) != "" ||
		strings.TrimSpace(placement.EvidenceRef) != "" || placement.ObservedAt != nil
}

// ValidatePlacement enforces the disjoint placement shapes. In particular,
// Managed workloads cannot masquerade as servers and a server placement cannot
// exist without the exact ServerID to which it is bound. A server placement
// never carries EvidenceRef or ObservedAt: the ServerID binding is the
// placement, and the service's runtime observation on that server is its
// evidence (placement freshness is derived from it on read).
func ValidatePlacement(serverID string, placement Placement) error {
	serverID = strings.TrimSpace(serverID)
	placement = NormalizePlacement(serverID, placement)
	managedFieldsEmpty := func() bool {
		return placement.ProviderID == "" && placement.ManagedTargetRef == "" &&
			placement.ProviderReceiptRef == "" && placement.SLAPolicyRef == "" &&
			placement.BackupPolicyRef == "" && placement.EvidenceRef == "" && placement.ObservedAt == nil &&
			placement.IsolationClass == "" && placement.IngressHostname == "" && placement.ManagedServiceSlotRef == ""
	}
	switch placement.TargetKind {
	case TargetKindServer:
		if serverID == "" || !managedFieldsEmpty() {
			return fmt.Errorf("server placement requires server_id and no managed-workload fields")
		}
		return nil
	case TargetKindManagedWorkload:
		if serverID != "" || placement.ProviderID == "" || placement.ManagedTargetRef == "" ||
			placement.ProviderReceiptRef == "" ||
			placement.BackupPolicyRef == "" || placement.EvidenceRef == "" ||
			placement.ObservedAt == nil || placement.ObservedAt.IsZero() {
			return fmt.Errorf("managed_workload requires no server_id plus provider target, receipt, backup, and observed evidence")
		}
		return validateManagedWorkloadPolicy(placement)
	case TargetKindUnknown:
		if serverID != "" || !managedFieldsEmpty() {
			return fmt.Errorf("unknown placement cannot carry server or managed-workload fields")
		}
		return nil
	default:
		return fmt.Errorf("unknown service target kind %q", placement.TargetKind)
	}
}

var (
	managedIngressHostname = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.kombify\.me$`)
	opaquePlacementRef     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
)

func validateManagedWorkloadPolicy(placement Placement) error {
	switch placement.IsolationClass {
	case "firecracker", "gvisor", "incus":
	default:
		return fmt.Errorf("managed_workload requires firecracker, gvisor, or incus isolation")
	}
	if !validPlacementRef(placement.BackupPolicyRef, "r2-backup-policy://") {
		return fmt.Errorf("managed_workload requires an opaque R2 backup policy reference")
	}
	if !validPlacementRef(placement.ManagedServiceSlotRef, "managed-service-slot://") {
		return fmt.Errorf("managed_workload requires a managed service slot reference")
	}
	if placement.SLAPolicyRef != "no-sla" {
		return fmt.Errorf("managed_workload requires the no-sla policy until the shared lane has an admitted SLA")
	}
	if placement.IngressHostname != "" && !managedIngressHostname.MatchString(placement.IngressHostname) {
		return fmt.Errorf("managed_workload public ingress must be a single label under kombify.me")
	}
	return nil
}

func validPlacementRef(ref, prefix string) bool {
	value, ok := strings.CutPrefix(ref, prefix)
	return ok && opaquePlacementRef.MatchString(value)
}

// PlacementEqual compares the persisted form, including evidence time.
func PlacementEqual(serverID string, left, right Placement) bool {
	left = NormalizePlacement(serverID, left)
	right = NormalizePlacement(serverID, right)
	if left.TargetKind != right.TargetKind || left.ProviderID != right.ProviderID ||
		left.ManagedTargetRef != right.ManagedTargetRef || left.ProviderReceiptRef != right.ProviderReceiptRef ||
		left.SLAPolicyRef != right.SLAPolicyRef || left.BackupPolicyRef != right.BackupPolicyRef ||
		left.IsolationClass != right.IsolationClass || left.IngressHostname != right.IngressHostname ||
		left.ManagedServiceSlotRef != right.ManagedServiceSlotRef ||
		left.EvidenceRef != right.EvidenceRef {
		return false
	}
	if left.ObservedAt == nil || right.ObservedAt == nil {
		return left.ObservedAt == right.ObservedAt
	}
	return left.ObservedAt.UTC().Equal(right.ObservedAt.UTC())
}

func normalizePlacementValue(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
