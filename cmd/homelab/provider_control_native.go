package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/providercontrol"
	"github.com/kombifyio/homelab/internal/providercontroljobs"
	"github.com/kombifyio/homelab/internal/providerevidence"
	"github.com/kombifyio/homelab/internal/substrate"
	"github.com/kombifyio/homelab/internal/substrateruntime"
	"github.com/kombifyio/homelab/pkg/agentcontrol"
	"github.com/kombifyio/homelab/pkg/config"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/logger"
	"github.com/kombifyio/homelab/pkg/vmleases"
	providerexecutor "github.com/kombifyio/stackkits/contracts/providerexecutor/v1beta1"
)

// managedProviderAdapter is the contract every executable managed provider
// must satisfy to open mutations: the at-most-once executor plus the opaque
// custody authority that native admission materializes per tenant subject.
type managedProviderAdapter interface {
	providercontrol.ProviderCreateRegionResolver
	providercontrol.AtMostOnceProvisionExecutor
	providercontrol.ManagedProviderSafetyReporter
	ManagedCredentialAuthority() providercontrol.ManagedCredentialAuthority
}

// managedProviderRegistration binds one adapter build to the provider identity
// whose catalog coverage it must satisfy before it may serve the Wizard.
type managedProviderRegistration struct {
	providerID          string
	adapterID           string
	adapterManifestHash string
	executionOfferingID string
	build               func() (managedProviderAdapter, error)
}

func enforceProviderCreateSafety(
	policy providercontrol.StaticProviderCreatePolicy,
	capabilities providercontrol.ProviderSafetyCapabilities,
) {
	if policy == nil || capabilities.ProvisionCandidateDiscovery {
		return
	}
	policy[capabilities.ProviderID] = providercontrol.ProviderCreateDecision{
		Enabled: false, ReasonCode: providercontrol.ProviderCreateReasonSafetyIncomplete,
	}
}

// nativeProviderControl is one composed provider-control runtime. The serving
// process uses runtime and actions; the managed-runtime retenant command uses
// admission without starting any worker.
type nativeProviderControl struct {
	runtime   *providercontrol.Runtime
	actions   jobs.RuntimeActions
	admission *providercontrol.NativeAdmission
}

// composeNativeProviderControl is shared by private and public selfhost builds.
// Commercial adapters and observers are injected only by the private wrapper.
func composeNativeProviderControl(database *sql.DB, edition config.Edition, log *logger.Logger,
	registrations []managedProviderRegistration, onParkedOperation func(providercontrol.OperationRef),
	observer func(*providercontrol.AsyncRequestStore) func(providerexecutor.Command, providerexecutor.Receipt, providerexecutor.Receipt),
) (*providercontrol.Runtime, jobs.RuntimeActions, error) {
	composed, err := composeNativeProviderControlParts(database, edition, log, registrations, onParkedOperation, observer)
	return composed.runtime, composed.actions, err
}

func composeNativeProviderControlParts(database *sql.DB, edition config.Edition, log *logger.Logger,
	registrations []managedProviderRegistration, onParkedOperation func(providercontrol.OperationRef),
	observer func(*providercontrol.AsyncRequestStore) func(providerexecutor.Command, providerexecutor.Receipt, providerexecutor.Receipt),
) (nativeProviderControl, error) {
	if database == nil {
		return nativeProviderControl{}, fmt.Errorf("compose provider control: database is required")
	}
	capacityPolicy, err := newProviderControlCapacityPolicy(edition)
	if err != nil {
		return nativeProviderControl{}, err
	}
	registry := providercontrol.NewRegistry()
	substrateAdapter, err := substrateruntime.NewAdapter(database, agentcontrol.NewHub(database))
	if err != nil {
		return nativeProviderControl{}, err
	}
	if err := registry.Register(substrate.AdapterID, substrateAdapter); err != nil {
		return nativeProviderControl{}, err
	}
	var gate providercontrol.MutationActivationGate = providercontrol.CertifiedMutationGate{Registry: registry}
	createPolicy := providercontrol.StaticProviderCreatePolicy{}
	managedCredentialAuthorities := providercontrol.ManagedCredentialAuthoritySet{}
	for _, provider := range registrations {
		// A registered provider is create-ready by default. Adapter composition
		// and capability discovery below are the authority; there is no second
		// deployment switch that can silently disable the normal lifecycle.
		primary := provider.executionOfferingID == ""
		if primary {
			createPolicy[provider.providerID] = providercontrol.ProviderCreateDecision{Enabled: true}
		}
		adapter, adapterErr := provider.build()
		if adapterErr != nil {
			if primary {
				createPolicy[provider.providerID] = providercontrol.ProviderCreateDecision{
					Enabled: false, ReasonCode: providercontrol.ProviderCreateReasonSafetyIncomplete,
				}
			}
			// A provider without resolvable custody stays unregistered rather
			// than blocking the ones that do resolve. Its wizard offerings then
			// fail closed on the missing adapter, not on a half-open path.
			if log != nil {
				log.Warn("provider_adapter_unavailable", "provider", provider.providerID, "reason", adapterErr)
			}
			continue
		}
		capabilities := adapter.ProviderSafetyCapabilities()
		if capabilityErr := capabilities.Validate(provider.providerID, provider.adapterID); capabilityErr != nil {
			return nativeProviderControl{}, fmt.Errorf(
				"validate %s provider adapter safety capabilities: %w", provider.providerID, capabilityErr)
		}
		if primary && !capabilities.ProvisionCandidateDiscovery {
			// Keep the adapter registered for polling and exact-handle cleanup,
			// but do not admit a create whose ambiguous outcome cannot be
			// discovered and resolved without guessing.
			enforceProviderCreateSafety(createPolicy, capabilities)
		}
		coverage := []providercontrol.CatalogCoverageRequirement{{
			ProviderID: provider.providerID, OfferingID: provider.executionOfferingID,
			AdapterID: provider.adapterID, AdapterManifestHash: provider.adapterManifestHash,
			ProvisionDispatchMode: providercontrol.ProvisionDispatchAtMostOnceManualReconcile,
		}}
		if primary {
			var err error
			coverage, err = providercontrol.ManagedCatalogCoverageRequirements(
				provider.providerID, provider.adapterID, provider.adapterManifestHash)
			if err != nil {
				return nativeProviderControl{}, fmt.Errorf("resolve %s provider catalog coverage: %w", provider.providerID, err)
			}
		}
		coverageCtx, cancelCoverage := context.WithTimeout(context.Background(), 5*time.Second)
		coverageErr := providercontrol.ValidateActiveCatalogCoverage(
			coverageCtx,
			database,
			coverage,
		)
		cancelCoverage()
		if coverageErr != nil {
			if !primary {
				if log != nil {
					log.Warn("provider_execution_variant_unavailable", "provider", provider.providerID,
						"adapter", provider.adapterID, "reason", coverageErr)
				}
				continue
			}
			return nativeProviderControl{}, fmt.Errorf(
				"validate %s provider catalog coverage: %w", provider.providerID, coverageErr)
		}
		if registerErr := registry.RegisterAtMostOnceProvision(provider.adapterID, adapter); registerErr != nil {
			return nativeProviderControl{}, fmt.Errorf(
				"register %s provider adapter: %w", provider.providerID, registerErr)
		}
		if log != nil {
			log.Info("provider_adapter_registered",
				"provider", provider.providerID,
				"adapter", provider.adapterID,
				"adapter_manifest_hash", provider.adapterManifestHash,
				"provision_dispatch_mode", providercontrol.ProvisionDispatchAtMostOnceManualReconcile,
				"candidate_discovery", capabilities.ProvisionCandidateDiscovery,
				"exact_handle_decommission", capabilities.ExactHandleDecommission,
				"definitive_absence_proof", capabilities.DefinitiveAbsenceProof,
				"host_firewall_baseline", capabilities.HostFirewallBaseline,
			)
		}
		gate = providercontrol.CertifiedMutationGate{Registry: registry}
		if primary {
			managedCredentialAuthorities[provider.providerID] = adapter.ManagedCredentialAuthority()
		}
	}
	profiles, err := providercontrol.NewPostgresCatalogExecutionProfileResolver(database)
	if err != nil {
		return nativeProviderControl{}, fmt.Errorf("compose provider catalog resolver: %w", err)
	}
	// FailClosedEvidenceVerifier stood here and rejected every envelope, so no
	// decommission could ever seal an absence receipt and no managed-runtime
	// capacity slot was ever released. The absence verifier resolves an
	// evidence reference back to the provider read the adapter recorded, which
	// is a real check rather than a blanket refusal.
	absenceVerifier, err := providerevidence.NewVerifier(database)
	if err != nil {
		return nativeProviderControl{}, fmt.Errorf("compose provider absence verifier: %w", err)
	}
	asyncRequests, err := providercontrol.NewAsyncRequestStore(database)
	if err != nil {
		return nativeProviderControl{}, fmt.Errorf("compose provider async request store: %w", err)
	}
	const provisionResolutionSubject = "system:provider-control-worker"
	var terminalObserver func(providerexecutor.Command, providerexecutor.Receipt, providerexecutor.Receipt)
	if observer != nil {
		terminalObserver = observer(asyncRequests)
	}
	runtime, err := providercontrol.NewRuntime(providercontrol.RuntimeConfig{
		Database:                database,
		Registry:                registry,
		ProfileResolver:         profiles,
		ActivationGate:          gate,
		EvidenceVerifier:        absenceVerifier,
		TerminalFailureObserver: terminalObserver,

		OnWorkerError: func(workerErr error) {
			if log != nil {
				log.Warn("provider_control_worker_blocked", "error", workerErr)
			}
		},
		AutomaticProvisionResolutionSubject: provisionResolutionSubject,
		// A parked provision advances no further on its own. Without this the
		// stall is indistinguishable from an idle queue in the logs.
		OnWorkerParked: func(operation providercontrol.OperationRef) {
			if log != nil {
				log.Warn("provider_control_manual_reconcile_required",
					"tenant", operation.TenantID, "operation", operation.OperationID)
			}
			if onParkedOperation != nil {
				onParkedOperation(operation)
			}
		},
	})
	if err != nil {
		return nativeProviderControl{}, fmt.Errorf("compose provider control runtime: %w", err)
	}
	admission, err := providercontrol.NewNativeAdmission(providercontrol.NativeAdmissionConfig{
		Database: database, Coordinator: runtime.Coordinator(), ActivationGate: gate,
		ProviderCreates: createPolicy,
		CapacityPolicy:  capacityPolicy, ManagedCredentialAuthorities: managedCredentialAuthorities,
	})
	if err != nil {
		return nativeProviderControl{}, fmt.Errorf("compose native provider admission: %w", err)
	}
	decommissioner, err := providercontroljobs.NewNativeDecommissioner(providercontroljobs.NativeDecommissionConfig{
		Database: database, Application: runtime.Application(), Ledger: runtime.Ledger(),
		ProvisionResolution: runtime, ResolutionSubject: provisionResolutionSubject,
		ReservedGuestAbsence: substrateAdapter,
	})
	if err != nil {
		return nativeProviderControl{}, fmt.Errorf("compose native provider decommission: %w", err)
	}
	power, err := providercontroljobs.NewNativePowerController(providercontroljobs.NativePowerConfig{
		Database: database, Application: runtime.Application(),
	})
	if err != nil {
		return nativeProviderControl{}, fmt.Errorf("compose native provider power control: %w", err)
	}
	managedDay2.setPower(power)
	manager, err := providercontroljobs.NewManager(providercontroljobs.ManagerConfig{
		Admission: admission, ActivationGate: gate, Decommissioner: decommissioner,
		AsyncRequests: asyncRequests,
	})
	if err != nil {
		return nativeProviderControl{}, fmt.Errorf("compose provider-control job manager: %w", err)
	}
	staleCapacityRecovery, err := providercontroljobs.NewStaleCapacityRecovery(
		providercontroljobs.StaleCapacityRecoveryConfig{
			Database: database, Decommissioner: manager,
			OnError: func(recoveryErr error) {
				if log != nil {
					log.Warn("provider_control_stale_capacity_recovery_blocked", "error", recoveryErr)
				}
			},
		},
	)
	if err != nil {
		return nativeProviderControl{}, fmt.Errorf("compose stale managed capacity recovery: %w", err)
	}
	if err := runtime.RegisterAuxiliaryWorker(staleCapacityRecovery.Run); err != nil {
		return nativeProviderControl{}, fmt.Errorf("register stale managed capacity recovery: %w", err)
	}
	if !neverEnrolledRecoveryDisabled() {
		neverEnrolledRecovery, neverEnrolledErr := providercontroljobs.NewNeverEnrolledRecovery(
			providercontroljobs.NeverEnrolledRecoveryConfig{
				Database: database, Decommissioner: manager,
				EnrollmentWindow: neverEnrolledRecoveryWindowFromEnv(log),
				OnReap: func(candidate providercontroljobs.NeverEnrolledRecoveryCandidate) {
					if log != nil {
						log.Warn("provider_control_never_enrolled_runtime_reaping",
							"tenant_id", candidate.TenantID,
							"stack_id", candidate.StackID,
							"lease_id", candidate.LeaseID,
							"reserved_at", candidate.ReservedAt.UTC().Format(time.RFC3339),
							"age", time.Since(candidate.ReservedAt).Round(time.Minute).String(),
						)
					}
				},
				OnError: func(recoveryErr error) {
					if log != nil {
						log.Warn("provider_control_never_enrolled_runtime_recovery_blocked", "error", recoveryErr)
					}
				},
			},
		)
		if neverEnrolledErr != nil {
			return nativeProviderControl{}, fmt.Errorf("compose never-enrolled managed runtime recovery: %w", neverEnrolledErr)
		}
		if err := runtime.RegisterAuxiliaryWorker(neverEnrolledRecovery.Run); err != nil {
			return nativeProviderControl{}, fmt.Errorf("register never-enrolled managed runtime recovery: %w", err)
		}
	}
	if err := registerManagedLeaseRenewal(runtime, database, edition, log); err != nil {
		return nativeProviderControl{}, err
	}
	return nativeProviderControl{
		runtime:   runtime,
		actions:   jobs.RuntimeActions{LeaseManager: manager, LeaseDecommissioner: manager},
		admission: admission,
	}, nil
}

// neverEnrolledRecoveryDisabled reports the operator kill switch. The reaper
// is enabled by default; disabling it is an incident-control action, never a
// quiet configuration.
func neverEnrolledRecoveryDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("HOMELAB_NEVER_ENROLLED_RECOVERY_DISABLED"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// neverEnrolledRecoveryWindowFromEnv reads the bounded enrollment window. An
// unparsable or out-of-range value falls back to the default with a warning
// instead of failing startup; the constructor still enforces the bounds.
func neverEnrolledRecoveryWindowFromEnv(log *logger.Logger) time.Duration {
	raw := strings.TrimSpace(os.Getenv("HOMELAB_NEVER_ENROLLED_RECOVERY_WINDOW"))
	if raw == "" {
		return 0
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed < providercontroljobs.MinimumNeverEnrolledRecoveryWindow || parsed > 24*time.Hour {
		if log != nil {
			log.Warn("never_enrolled_recovery_window_invalid", "value", raw, "fallback", "1h")
		}
		return 0
	}
	return parsed
}

// registerManagedLeaseRenewal keeps every managed lease active while its
// owner's subscription entitles it (platform-2xnsxf). Administration is the
// authority, so the loop runs only on the hosted edition with a service
// identity that can read it; elsewhere no managed subscription lease exists.
func registerManagedLeaseRenewal(runtime *providercontrol.Runtime, database *sql.DB, edition config.Edition, log *logger.Logger) error {
	if !edition.IsSaaS() {
		return nil
	}
	entitlements := newAdministrationEntitlements()
	if entitlements == nil {
		if log != nil {
			log.Warn("managed_lease_renewal_unavailable", "reason", "administration_entitlements_unreadable")
		}
		return nil
	}
	renewal, err := providercontroljobs.NewManagedLeaseRenewal(providercontroljobs.ManagedLeaseRenewalConfig{
		Database: database, Renewer: vmleases.NewPostgresStore(database), Entitlements: entitlements,
		OnEvent: func(event providercontroljobs.ManagedLeaseRenewalEvent) { logManagedLeaseRenewal(log, event) },
		OnError: func(renewalErr error) {
			if log != nil {
				log.Warn("managed_lease_renewal_blocked", "error", renewalErr)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("compose managed lease renewal: %w", err)
	}
	if err := runtime.RegisterAuxiliaryWorker(renewal.Run); err != nil {
		return fmt.Errorf("register managed lease renewal: %w", err)
	}
	return nil
}

// logManagedLeaseRenewal is the renewal receipt: identifiers and times only,
// never the person's entitlement list or a credential.
func logManagedLeaseRenewal(log *logger.Logger, event providercontroljobs.ManagedLeaseRenewalEvent) {
	if log == nil {
		return
	}
	attributes := []any{
		"tenant_id", event.Candidate.TenantID,
		"lease_id", event.Candidate.LeaseID,
		"provider_id", event.Candidate.ProviderID,
		"previous_valid_until", event.PreviousValidUntil.UTC().Format(time.RFC3339),
		"valid_until", event.ValidUntil.UTC().Format(time.RFC3339),
	}
	switch event.Outcome {
	case providercontroljobs.ManagedLeaseRenewalRenewed:
		log.Info("managed_lease_renewed", append(attributes, "repaired_lapse", event.Lapsed)...)
	case providercontroljobs.ManagedLeaseRenewalEntitlementUnknown:
		log.Warn("managed_lease_renewal_entitlement_unknown", attributes...)
	case providercontroljobs.ManagedLeaseRenewalNotEntitled:
		log.Info("managed_lease_renewal_not_entitled", append(attributes, "missing", strings.Join(event.Missing, ","))...)
	case providercontroljobs.ManagedLeaseRenewalLeaseChanged:
		log.Info("managed_lease_renewal_skipped_changed_lease", attributes...)
	case providercontroljobs.ManagedLeaseRenewalNotRenewable:
		// A lease being torn down or released stays listed until its row is
		// cancelled; it is checked every pass, so it is not logged above debug.
		log.Debug("managed_lease_renewal_not_renewable", attributes...)
	}
}
