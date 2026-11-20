package monthlyruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/runtimeproduct/serverruntime"
	"github.com/kombifyio/homelab/internal/runtimeproduct/vmlease"
	"github.com/kombifyio/homelab/pkg/vmleases"
)

// A governed decommission tears down a managed lease on behalf of an owner who
// can no longer do it because the owner's account is being erased: the Cloud
// data-subject coordinator delivers the confirmed erasure, service
// authenticated, as system work. It is the owner decommission with a
// different admission: the same generation claim, kombify.me address
// removal, native provider-control teardown and cleanup readback. No human
// request reaches it; the account-erasure cascade is its only caller.

// GovernedReason names why a lease is decommissioned without its owner.
type GovernedReason string

const (
	// GovernedReasonAccountErasure: the owner's confirmed account erasure
	// reached Homelab, which removes provider resources before erasing.
	GovernedReasonAccountErasure GovernedReason = "account_erasure"
)

const (
	// Lease metadata that records a governed admission next to the lease.
	MetadataKeyGovernedReason   = "provider_decommission_governed_reason"
	MetadataKeyGovernedActor    = "provider_decommission_governed_by"
	MetadataKeyGovernedEvidence = "provider_decommission_governed_evidence"
	MetadataKeyGovernedAt       = "provider_decommission_governed_at"

	// OperationEventGovernedDecommission is the durable journal record of a
	// governed admission: actor is the service actor, status the governed
	// reason. It is written before provider teardown is admitted.
	OperationEventGovernedDecommission = "governed_decommission"

	governedEvidenceErasurePrefix = "datasubject_erase_request:"
)

var (
	// ErrGovernedOwnerMismatch refuses a request whose named owner, tenant or
	// owner kind does not match the lease.
	ErrGovernedOwnerMismatch = errors.New("monthlyruntime: lease owner does not match the governed request")
	// ErrGovernedRequestInvalid refuses a malformed governed request.
	ErrGovernedRequestInvalid = errors.New("monthlyruntime: governed decommission request is invalid")
)

// LeaseInventoryLister lists every lease of one tenant with its execution
// authority. The account-erasure cascade uses it to find the erased subject's
// managed leases.
type LeaseInventoryLister interface {
	ListInventoryByTenant(ctx context.Context, tenantID string) ([]vmleases.LeaseInventoryRecord, error)
}

// governedDecommissionRequest asks for the governed decommission of one lease.
// TenantID, LeaseID and OwnerSubjectID must all name the same lease: the
// caller states whose lease it means and the service refuses any mismatch.
type governedDecommissionRequest struct {
	TenantID       string
	LeaseID        vmlease.LeaseID
	OwnerSubjectID string
	// Actor names who decommissions: "service:<name>".
	Actor  string
	Reason GovernedReason
	// ErasureRequestID is the data-subject request id.
	ErasureRequestID string
}

// governedAdmission travels inside ActionRequest. It is unexported, so only
// the erasure cascade can replace the owner-visibility check with it.
type governedAdmission struct {
	ownerSubjectID string
	actor          string
	reason         GovernedReason
	evidence       string
	admittedAt     time.Time
}

// metadata returns the governed record for the lease. The first governed
// admission of a generation stays recorded; a repeat call only adds its own
// journal event.
func (g *governedAdmission) metadata(current map[string]string) map[string]string {
	if strings.TrimSpace(current[MetadataKeyGovernedReason]) != "" {
		return nil
	}
	return map[string]string{
		MetadataKeyGovernedReason:   string(g.reason),
		MetadataKeyGovernedActor:    g.actor,
		MetadataKeyGovernedEvidence: g.evidence,
		MetadataKeyGovernedAt:       g.admittedAt.UTC().Format(time.RFC3339),
	}
}

// authorize replaces canAccessLease for a governed request: the lease must be
// the named user's lease in the named tenant, and the request must be a plain
// decommission (no internal continuation, no force).
func (g *governedAdmission) authorize(lease vmlease.Lease, req ActionRequest, tenantID string) error {
	if req.Action != serverruntime.RuntimeActionDecommission || req.Internal ||
		req.ReconcileClaimedDecommission || req.Force {
		return ErrForbidden
	}
	if strings.TrimSpace(lease.Subject.OrgID) != tenantID || lease.Subject.Kind != vmlease.SubjectUser ||
		strings.TrimSpace(lease.Subject.ID) != g.ownerSubjectID {
		return ErrGovernedOwnerMismatch
	}
	return nil
}

var (
	governedActorPattern     = regexp.MustCompile(`^service:[^\s]{1,256}$`)
	governedRequestIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// governedDecommission admits one lease of the erasure cascade and runs it
// through the owner decommission path. Repeating it for the same generation
// is idempotent: provider control keys its teardown on the exact resource
// generation.
func (s *Service) governedDecommission(ctx context.Context, req governedDecommissionRequest) error {
	admission, err := s.admitGoverned(req)
	if err != nil {
		return err
	}
	_, err = s.Action(ctx, ActionRequest{
		TenantID: strings.TrimSpace(req.TenantID),
		UserID:   admission.actor,
		LeaseID:  req.LeaseID,
		Action:   serverruntime.RuntimeActionDecommission,
		governed: admission,
	})
	return err
}

func (s *Service) admitGoverned(req governedDecommissionRequest) (*governedAdmission, error) {
	if s == nil || s.Leases == nil {
		return nil, vmleases.ErrEnrollmentRequired
	}
	admission := &governedAdmission{
		ownerSubjectID: strings.TrimSpace(req.OwnerSubjectID),
		actor:          strings.TrimSpace(req.Actor),
		reason:         req.Reason,
		admittedAt:     time.Now().UTC(),
	}
	erasureRequestID := strings.ToLower(strings.TrimSpace(req.ErasureRequestID))
	if strings.TrimSpace(req.TenantID) == "" || strings.TrimSpace(string(req.LeaseID)) == "" ||
		admission.ownerSubjectID == "" || len(admission.ownerSubjectID) > 256 ||
		req.Reason != GovernedReasonAccountErasure ||
		!governedActorPattern.MatchString(admission.actor) ||
		!governedRequestIDPattern.MatchString(erasureRequestID) {
		return nil, ErrGovernedRequestInvalid
	}
	// Governed teardown is only ever the native, durable provider-control
	// reconciliation; the legacy node-side path cannot prove provider absence.
	if !s.ForceDecommissionReady() {
		return nil, ErrReconciliationUnavailable
	}
	admission.evidence = governedEvidenceErasurePrefix + erasureRequestID
	return admission, nil
}

// recordGovernedAdmission writes the governed audit event strictly, right
// after the generation claim and before any address removal, replay
// finalization or provider teardown: no governed effect happens without a
// durable record naming its actor and reason. Every governed call is recorded.
func (s *Service) recordGovernedAdmission(ctx context.Context, tenantID string, leaseID vmlease.LeaseID, admission *governedAdmission, resourceGenerationDigest string) error {
	recorder, ok := s.Leases.(StrictOperationRecorder)
	if !ok {
		return ErrDecommissionJournalUnavailable
	}
	err := recorder.RecordOperationStrict(ctx, vmleases.OperationEvent{
		TenantID:                 tenantID,
		LeaseID:                  leaseID,
		EventType:                OperationEventGovernedDecommission,
		Status:                   string(admission.reason),
		Actor:                    admission.actor,
		ResourceGenerationDigest: resourceGenerationDigest,
	})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDecommissionJournalUnavailable, err)
	}
	slog.Info("governed_decommission_admitted",
		"tenant_id", tenantID, "lease_id", string(leaseID), "actor", admission.actor,
		"reason", string(admission.reason), "owner_evidence", admission.evidence)
	return nil
}

// DecommissionErasedSubjectLeases is the account-erasure cascade: it admits
// the governed decommission of every managed lease the erased subject still
// owns in one tenant whose decommission is not yet admitted. Leases already
// handed to provider control (desired absent) are left to its reconciler. It
// returns the admitted lease ids and every per-lease refusal joined, so one
// failing lease never hides the others.
func (s *Service) DecommissionErasedSubjectLeases(ctx context.Context, tenantID, subjectID, actor, erasureRequestID string) ([]string, error) {
	if s == nil || s.Leases == nil {
		return nil, vmleases.ErrEnrollmentRequired
	}
	tenantID, subjectID = strings.TrimSpace(tenantID), strings.TrimSpace(subjectID)
	if tenantID == "" || subjectID == "" {
		return nil, ErrGovernedRequestInvalid
	}
	lister, ok := s.Leases.(LeaseInventoryLister)
	if !ok {
		return nil, vmleases.ErrLeaseInventoryUnavailable
	}
	records, err := lister.ListInventoryByTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	var admitted []string
	var refusals []error
	for _, record := range records {
		lease := record.Lease
		if lease.Subject.Kind != vmlease.SubjectUser || strings.TrimSpace(lease.Subject.ID) != subjectID ||
			strings.TrimSpace(lease.Subject.OrgID) != tenantID || !IsMonthlyRuntimeMetadata(lease.Metadata) ||
			lease.DesiredState == vmlease.DesiredStateArchived {
			continue
		}
		if err := s.governedDecommission(ctx, governedDecommissionRequest{
			TenantID: tenantID, LeaseID: lease.ID, OwnerSubjectID: subjectID,
			Actor: actor, Reason: GovernedReasonAccountErasure, ErasureRequestID: erasureRequestID,
		}); err != nil {
			refusals = append(refusals, fmt.Errorf("lease %s: %w", lease.ID, err))
			continue
		}
		admitted = append(admitted, string(lease.ID))
	}
	return admitted, errors.Join(refusals...)
}
