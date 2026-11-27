package providercontroljobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kombifyio/homelab/internal/runtimeproduct/vmlease"
	"github.com/kombifyio/homelab/pkg/db/backgroundlock"
	"github.com/kombifyio/homelab/pkg/monthlyruntime"
	"github.com/kombifyio/homelab/pkg/releasechannel"
	"github.com/kombifyio/homelab/pkg/vmleases"
)

const (
	// DefaultManagedLeaseRenewalHorizon renews a lease once less than this much
	// of its window is left. Two weeks of a 30-day window survive a long
	// Administration outage before any lease lapses.
	DefaultManagedLeaseRenewalHorizon = 14 * 24 * time.Hour
	// MaximumManagedLeaseRenewalHorizon stays below the renewed window, so a
	// freshly renewed lease is not a candidate again.
	MaximumManagedLeaseRenewalHorizon   = 20 * 24 * time.Hour
	minimumManagedLeaseRenewalHorizon   = time.Hour
	defaultManagedLeaseRenewalInterval  = time.Hour
	defaultManagedLeaseRenewalBatch     = 25
	maximumManagedLeaseRenewalPerPass   = 200
	managedLeaseRenewalEntitlementCache = 256
)

// Managed lease renewal outcomes. They are logged; none carries a secret.
const (
	ManagedLeaseRenewalRenewed            = "renewed"
	ManagedLeaseRenewalCurrent            = "current"
	ManagedLeaseRenewalEntitlementUnknown = "entitlement_unknown"
	ManagedLeaseRenewalNotEntitled        = "not_entitled"
	ManagedLeaseRenewalLeaseChanged       = "lease_changed"
	ManagedLeaseRenewalNotRenewable       = "not_renewable"
)

// ManagedLeaseEntitlements reads a person's committed entitlement projection
// from Administration. An error means unknown, never none.
type ManagedLeaseEntitlements interface {
	Current(ctx context.Context, subject, tenantID string) ([]string, error)
}

// ManagedLeaseRenewer extends one exact lease generation's validity window.
// NativeRenewalEligible is the unlocked pre-check that spares the entitlement
// read for a lease that cannot renew; RenewNativeLease repeats it under locks.
type ManagedLeaseRenewer interface {
	NativeRenewalEligible(ctx context.Context, request vmleases.NativeRenewalRequest) (bool, error)
	RenewNativeLease(ctx context.Context, request vmleases.NativeRenewalRequest) (vmleases.NativeRenewalResult, error)
}

// ManagedLeaseRenewalCandidate is one live provider-control subscription lease
// whose validity window ends within the renewal horizon or already ended.
type ManagedLeaseRenewalCandidate struct {
	TenantID             string
	OwnerID              string
	LeaseID              string
	ProviderID           string
	ResourceGenerationID string
	ValidUntil           time.Time
}

// ManagedLeaseRenewalEvent reports one decision. Missing names the
// entitlement keys an answered projection lacked.
type ManagedLeaseRenewalEvent struct {
	Candidate          ManagedLeaseRenewalCandidate
	Outcome            string
	Missing            []string
	PreviousValidUntil time.Time
	ValidUntil         time.Time
	// Lapsed is true when the window had already ended before this renewal:
	// the renewal repaired provider control of a still-entitled lease.
	Lapsed bool
}

// ManagedLeaseRenewal keeps every managed lease active for as long as its
// owner's subscription entitles it. Administration is the authority: a lease
// renews only on an answered projection that grants every managed-runtime key
// its provider requires. It never cancels, stops or shortens anything: a lease
// whose owner lost the entitlement simply reaches the end of its window, and
// an unknown answer leaves the lease as it is until the next pass.
type ManagedLeaseRenewal struct {
	database     *sql.DB
	renewer      ManagedLeaseRenewer
	entitlements ManagedLeaseEntitlements
	horizon      time.Duration
	validFor     time.Duration
	batchSize    int
	interval     time.Duration
	onEvent      func(ManagedLeaseRenewalEvent)
	onError      func(error)
	lock         *backgroundlock.Locker
	cursorMu     sync.Mutex
	afterTenant  string
	afterLease   string
}

type ManagedLeaseRenewalConfig struct {
	Database     *sql.DB
	Renewer      ManagedLeaseRenewer
	Entitlements ManagedLeaseEntitlements
	// Horizon is how much of the window may be left before renewal. Zero
	// selects DefaultManagedLeaseRenewalHorizon.
	Horizon time.Duration
	// ValidFor is the renewed window from database time. Zero selects the
	// native admission window.
	ValidFor  time.Duration
	BatchSize int
	Interval  time.Duration
	OnEvent   func(ManagedLeaseRenewalEvent)
	OnError   func(error)
	// Lock holds one bounded sweep per replica set at a time. Nil runs the
	// sweep unlocked.
	Lock *backgroundlock.Locker
}

func NewManagedLeaseRenewal(cfg ManagedLeaseRenewalConfig) (*ManagedLeaseRenewal, error) {
	if cfg.Database == nil || nilManagerDependency(cfg.Renewer) || nilManagerDependency(cfg.Entitlements) {
		return nil, fmt.Errorf("providercontroljobs: managed lease renewal database, renewer and entitlements are required")
	}
	if cfg.Horizon == 0 {
		cfg.Horizon = DefaultManagedLeaseRenewalHorizon
	}
	if cfg.ValidFor == 0 {
		cfg.ValidFor = vmleases.DefaultNativeAdmissionValidity
	}
	if cfg.Horizon < minimumManagedLeaseRenewalHorizon || cfg.Horizon > MaximumManagedLeaseRenewalHorizon ||
		cfg.ValidFor > vmleases.MaxNativeAdmissionValidity || cfg.ValidFor <= cfg.Horizon {
		return nil, fmt.Errorf("providercontroljobs: managed lease renewal horizon must be between %s and %s and shorter than the renewed window",
			minimumManagedLeaseRenewalHorizon, MaximumManagedLeaseRenewalHorizon)
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = defaultManagedLeaseRenewalBatch
	}
	if cfg.BatchSize > 101 {
		return nil, fmt.Errorf("providercontroljobs: managed lease renewal batch must not exceed 101")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = defaultManagedLeaseRenewalInterval
	}
	return &ManagedLeaseRenewal{
		database: cfg.Database, renewer: cfg.Renewer, entitlements: cfg.Entitlements,
		horizon: cfg.Horizon, validFor: cfg.ValidFor, batchSize: cfg.BatchSize, interval: cfg.Interval,
		onEvent: cfg.OnEvent, onError: cfg.OnError, lock: cfg.Lock,
	}, nil
}

// Run performs one pass at boot and then repeats on the configured interval
// until the context is canceled. The boot pass repairs leases that lapsed
// while their owner stayed entitled.
func (r *ManagedLeaseRenewal) Run(ctx context.Context) {
	if r == nil {
		return
	}
	r.runPass(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runPass(ctx)
		}
	}
}

// runPass holds the lock across the whole bounded sweep, so the in-memory
// keyset cursor advances only while this replica is the one sweeping. A pass
// skipped because another replica swept resets the cursor; a renewal repeated
// from the start is fenced by its exact lease generation and is idempotent.
func (r *ManagedLeaseRenewal) runPass(ctx context.Context) {
	runLockedPass(ctx, r.lock, managedLeaseRenewalLock, r.RenewOnce, r.resetCursor, r.onError)
}

func (r *ManagedLeaseRenewal) resetCursor() {
	r.cursorMu.Lock()
	defer r.cursorMu.Unlock()
	r.afterTenant, r.afterLease = "", ""
}

type managedLeaseEntitlementAnswer struct {
	granted []string
	err     error
}

// RenewOnce works through at most the bounded per-pass budget in tenant/lease
// order with a resumable keyset cursor. One owner's projection is read once
// per pass.
func (r *ManagedLeaseRenewal) RenewOnce(ctx context.Context) error {
	if r == nil || r.database == nil || r.renewer == nil || r.entitlements == nil {
		return fmt.Errorf("providercontroljobs: managed lease renewal is not configured")
	}
	r.cursorMu.Lock()
	defer r.cursorMu.Unlock()
	answers := map[string]managedLeaseEntitlementAnswer{}
	afterTenant, afterLease := r.afterTenant, r.afterLease
	processed := 0
	for processed < maximumManagedLeaseRenewalPerPass {
		limit := r.batchSize
		if remaining := maximumManagedLeaseRenewalPerPass - processed; limit > remaining {
			limit = remaining
		}
		candidates, err := r.loadCandidates(ctx, afterTenant, afterLease, limit)
		if err != nil {
			return err
		}
		if len(candidates) == 0 {
			r.afterTenant, r.afterLease = "", ""
			return nil
		}
		for _, candidate := range candidates {
			afterTenant, afterLease = candidate.TenantID, candidate.LeaseID
			r.afterTenant, r.afterLease = afterTenant, afterLease
			processed++
			// The other release channel runs this tenant's work (pkg/releasechannel).
			if !releasechannel.OwnsTenant(candidate.TenantID) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			request := r.renewalRequest(candidate)
			eligible, err := r.renewer.NativeRenewalEligible(ctx, request)
			if err != nil {
				if r.onError != nil && !errors.Is(err, context.Canceled) {
					r.onError(fmt.Errorf("providercontroljobs: check managed lease %s renewal: %w", candidate.LeaseID, err))
				}
				continue
			}
			if !eligible {
				r.emit(ManagedLeaseRenewalEvent{
					Candidate: candidate, Outcome: ManagedLeaseRenewalNotRenewable,
					PreviousValidUntil: candidate.ValidUntil, ValidUntil: candidate.ValidUntil,
				})
				continue
			}
			key := candidate.OwnerID + "\x00" + candidate.TenantID
			answer, cached := answers[key]
			if !cached {
				answer.granted, answer.err = r.entitlements.Current(ctx, candidate.OwnerID, candidate.TenantID)
				if len(answers) < managedLeaseRenewalEntitlementCache {
					answers[key] = answer
				}
			}
			r.renewCandidate(ctx, candidate, request, answer)
		}
		if len(candidates) < limit {
			r.afterTenant, r.afterLease = "", ""
			return nil
		}
	}
	return nil
}

func (r *ManagedLeaseRenewal) renewalRequest(candidate ManagedLeaseRenewalCandidate) vmleases.NativeRenewalRequest {
	return vmleases.NativeRenewalRequest{
		TenantID: candidate.TenantID, LeaseID: vmlease.LeaseID(candidate.LeaseID),
		OwnerSubjectID: candidate.OwnerID, ResourceGenerationID: candidate.ResourceGenerationID,
		ValidFor: r.validFor,
	}
}

func (r *ManagedLeaseRenewal) renewCandidate(
	ctx context.Context,
	candidate ManagedLeaseRenewalCandidate,
	request vmleases.NativeRenewalRequest,
	answer managedLeaseEntitlementAnswer,
) {
	event := ManagedLeaseRenewalEvent{Candidate: candidate, PreviousValidUntil: candidate.ValidUntil, ValidUntil: candidate.ValidUntil}
	if answer.err != nil {
		event.Outcome = ManagedLeaseRenewalEntitlementUnknown
		r.emit(event)
		return
	}
	decision := monthlyruntime.EvaluateManagedRuntimeRenewal(candidate.ProviderID, answer.granted)
	if decision.Denied {
		event.Outcome = ManagedLeaseRenewalNotEntitled
		event.Missing = decision.MissingFeatures
		r.emit(event)
		return
	}
	result, err := r.renewer.RenewNativeLease(ctx, request)
	if errors.Is(err, vmleases.ErrNativeRenewalRefused) {
		event.Outcome = ManagedLeaseRenewalLeaseChanged
		r.emit(event)
		return
	}
	if err != nil {
		if r.onError != nil && !errors.Is(err, context.Canceled) {
			r.onError(fmt.Errorf("providercontroljobs: renew managed lease %s: %w", candidate.LeaseID, err))
		}
		return
	}
	event.PreviousValidUntil, event.ValidUntil = result.PreviousValidUntil, result.ValidUntil
	event.Lapsed = !result.DatabaseTime.Before(result.PreviousValidUntil)
	event.Outcome = ManagedLeaseRenewalCurrent
	if result.Renewed {
		event.Outcome = ManagedLeaseRenewalRenewed
	}
	r.emit(event)
}

func (r *ManagedLeaseRenewal) emit(event ManagedLeaseRenewalEvent) {
	if r.onEvent != nil {
		r.onEvent(event)
	}
}

func (r *ManagedLeaseRenewal) loadCandidates(
	ctx context.Context,
	afterTenant string,
	afterLease string,
	limit int,
) ([]ManagedLeaseRenewalCandidate, error) {
	rows, err := r.database.QueryContext(ctx, `
		SELECT tenant_id, owner_subject_id, lease_id, provider_id, resource_generation_id::text, valid_until
		FROM provider_control_list_managed_lease_renewal_candidates($1, $2, $3, $4)
	`, strings.TrimSpace(afterTenant), strings.TrimSpace(afterLease), int(r.horizon.Seconds()), limit)
	if err != nil {
		return nil, fmt.Errorf("providercontroljobs: list managed lease renewal candidates: %w", err)
	}
	defer rows.Close()
	candidates := make([]ManagedLeaseRenewalCandidate, 0, limit)
	for rows.Next() {
		var candidate ManagedLeaseRenewalCandidate
		if err := rows.Scan(&candidate.TenantID, &candidate.OwnerID, &candidate.LeaseID, &candidate.ProviderID,
			&candidate.ResourceGenerationID, &candidate.ValidUntil); err != nil {
			return nil, fmt.Errorf("providercontroljobs: scan managed lease renewal candidate: %w", err)
		}
		if candidate.TenantID == "" || candidate.OwnerID == "" || candidate.LeaseID == "" ||
			candidate.ProviderID == "" || candidate.ResourceGenerationID == "" || candidate.ValidUntil.IsZero() {
			return nil, fmt.Errorf("providercontroljobs: managed lease renewal candidate is incomplete")
		}
		candidate.ValidUntil = candidate.ValidUntil.UTC()
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("providercontroljobs: iterate managed lease renewal candidates: %w", err)
	}
	return candidates, nil
}
