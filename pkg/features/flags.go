// Package features provides feature flag management for kombifyTechstack.
// Signed Gateway decisions are authoritative; offline editions use shipped defaults.
// Consent is enforced separately from flag decisions.
package features

import (
	"context"
	"fmt"
	"strings"

	"github.com/kombifyio/techstack/internal/gocommon/edgeauth"
	"github.com/kombifyio/techstack/internal/runtimeproduct/serverruntime"
	"github.com/kombifyio/techstack/pkg/config"
	"github.com/kombifyio/techstack/pkg/logger"
)

// RiskLevel indicates the security risk level of a feature
type RiskLevel string

const (
	RiskLevelLow    RiskLevel = "low"
	RiskLevelMedium RiskLevel = "medium"
	RiskLevelHigh   RiskLevel = "high"
)

// Category classifies features by their type
type Category string

const (
	CategorySecurity Category = "security"
	CategoryBeta     Category = "beta"
	CategoryUX       Category = "ux"
)

// FeatureDefinition describes a feature flag with its metadata
type FeatureDefinition struct {
	Key             string    `json:"key"`
	Name            string    `json:"name"`
	DefaultValue    bool      `json:"default_value"`
	RequiresConsent bool      `json:"requires_consent"`
	RequiresAdmin   bool      `json:"requires_admin"`
	RiskLevel       RiskLevel `json:"risk_level"`
	Description     string    `json:"description"`
	Category        Category  `json:"category"`
}

// FlagState represents the current state of a feature flag for a user
type FlagState struct {
	Key             string    `json:"key"`
	Name            string    `json:"name"`
	Enabled         bool      `json:"enabled"`
	Locked          bool      `json:"locked"` // true if the feature needs the self-hosted instance owner and the caller is not that owner
	RequiresConsent bool      `json:"requires_consent"`
	HasConsent      bool      `json:"has_consent"`
	RiskLevel       RiskLevel `json:"risk_level"`
	Description     string    `json:"description"`
	Category        Category  `json:"category"`
}

// securityFeatures defines all security-critical features (Default: OFF - must be explicitly enabled).
var securityFeatures = map[string]FeatureDefinition{
	"network_discovery": {
		Key:             "network_discovery_enabled",
		Name:            "Network Discovery",
		DefaultValue:    false,
		RequiresConsent: true,
		RequiresAdmin:   false,
		RiskLevel:       RiskLevelHigh,
		Description:     "Scans local network for devices and open ports. May trigger security alerts on corporate networks.",
		Category:        CategorySecurity,
	},
	"raw_commands": {
		Key:             "raw_command_execution",
		Name:            "Raw Command Execution",
		DefaultValue:    false,
		RequiresConsent: true,
		RequiresAdmin:   true,
		RiskLevel:       RiskLevelHigh,
		Description:     "Execute arbitrary shell commands on connected nodes. Use with extreme caution.",
		Category:        CategorySecurity,
	},
	"ssh_tunnel": {
		Key:             "ssh_tunnel_creation",
		Name:            "SSH Tunnel Creation",
		DefaultValue:    false,
		RequiresConsent: true,
		RequiresAdmin:   true,
		RiskLevel:       RiskLevelHigh,
		Description:     "Create SSH tunnels to nodes for remote access. Opens network connections.",
		Category:        CategorySecurity,
	},
	"cloud_backup": {
		Key:             "cloud_backup_enabled",
		Name:            "Cloud Backup",
		DefaultValue:    false,
		RequiresConsent: true,
		RequiresAdmin:   false,
		RiskLevel:       RiskLevelMedium,
		Description:     "Backup data to external S3-compatible storage. Data leaves local network.",
		Category:        CategorySecurity,
	},
}

// betaFeatures defines experimental features. Each definition owns its default.
var betaFeatures = map[string]FeatureDefinition{
	"ionos_standard_photo_memory": {
		Key: "ionos_standard_photo_memory", Name: "IONOS Standard photo memory", DefaultValue: false,
		RiskLevel: RiskLevelLow, Category: CategoryBeta,
		Description: "Select the IONOS Memory Cube S for full Immich in the Standard slot.",
	},
	"server_hardware_fit": {
		Key: "server_hardware_fit", Name: "Server hardware fit", DefaultValue: false,
		RiskLevel: RiskLevelLow, Category: CategoryBeta,
		Description: "Show selected workload capacity evidence at server choice.",
	},
	"cloudflare_tunnel": {
		Key:             "cloudflare_tunnel_enabled",
		Name:            "Cloudflare Tunnel",
		DefaultValue:    false,
		RequiresConsent: false,
		RequiresAdmin:   false,
		RiskLevel:       RiskLevelMedium,
		Description:     "Quick Tunnel for worker registration without port forwarding. Beta feature.",
		Category:        CategoryBeta,
	},
	"self_healing": {
		Key:             "self_healing_enabled",
		Name:            "Automatic Remediation",
		DefaultValue:    false,
		RequiresConsent: false,
		RequiresAdmin:   true,
		RiskLevel:       RiskLevelMedium,
		Description:     "Rule-based problem detection and remediation workflows. Experimental and off by default.",
		Category:        CategoryBeta,
	},
	"ha_stackkit": {
		Key:             "stackkit_ha_enabled",
		Name:            "HA Homelab StackKit",
		DefaultValue:    false,
		RequiresConsent: false,
		RequiresAdmin:   false,
		RiskLevel:       RiskLevelLow,
		Description:     "Kubernetes-based high-availability StackKit. Beta feature.",
		Category:        CategoryBeta,
	},
	"custom_domains": {
		Key:             "custom_domains_enabled",
		Name:            "Custom Domains",
		DefaultValue:    false,
		RequiresConsent: false,
		RequiresAdmin:   false,
		RiskLevel:       RiskLevelLow,
		Description:     "Custom domain mapping for deployed stacks. Requires PRO tier or above.",
		Category:        CategoryBeta,
	},
	"auto_scaling": {
		Key:             "auto_scaling_enabled",
		Name:            "Auto Scaling",
		DefaultValue:    false,
		RequiresConsent: false,
		RequiresAdmin:   false,
		RiskLevel:       RiskLevelMedium,
		Description:     "Automatic resource scaling for stacks. Requires ENTERPRISE tier.",
		Category:        CategoryBeta,
	},
	"monthly_runtime": {
		Key:             serverruntime.FeatureTechStackMonthlyRuntime,
		Name:            "Monthly Runtime",
		DefaultValue:    false,
		RequiresConsent: false,
		RequiresAdmin:   false,
		RiskLevel:       RiskLevelMedium,
		Description:     "Enables subscription-based monthly server runtime orchestration.",
		Category:        CategoryBeta,
	},
	"monthly_runtime_centron": {
		Key:             serverruntime.FeatureTechStackMonthlyRuntimeCentron,
		Name:            "Monthly Runtime: Centron",
		DefaultValue:    false,
		RequiresConsent: false,
		RequiresAdmin:   false,
		RiskLevel:       RiskLevelMedium,
		Description:     "Enables Centron as a TechStack Monthly Runtime provider.",
		Category:        CategoryBeta,
	},
	"monthly_runtime_ionos": {
		Key:             serverruntime.FeatureTechStackMonthlyRuntimeIONOS,
		Name:            "Monthly Runtime: IONOS",
		DefaultValue:    false,
		RequiresConsent: false,
		RequiresAdmin:   false,
		RiskLevel:       RiskLevelMedium,
		Description:     "Enables IONOS as a TechStack Monthly Runtime provider.",
		Category:        CategoryBeta,
	},
	"monthly_runtime_cloudkit": {
		Key:             edgeFlagTechStackManagedRuntimeCloudKit,
		Name:            "Monthly Runtime: Cloud Kit",
		DefaultValue:    false,
		RequiresConsent: false,
		RequiresAdmin:   false,
		RiskLevel:       RiskLevelMedium,
		Description:     "Enables Cloud Kit deployments on TechStack Monthly Runtime.",
		Category:        CategoryBeta,
	},
	// Backup entitlements ride the signed edge chain like the monthly runtime
	// keys (pkg/monthlyruntime/backup.go). Without a definition every check
	// errored before reading that chain, so entitled accounts were denied.
	"backup_config": {
		Key:             edgeFlagTechStackBackupConfig,
		Name:            "Backups: Configuration",
		DefaultValue:    false,
		RequiresConsent: false,
		RequiresAdmin:   false,
		RiskLevel:       RiskLevelMedium,
		Description:     "Enables configuration backups for managed stacks.",
		Category:        CategoryBeta,
	},
	"backup_content": {
		Key:             edgeFlagTechStackBackupContent,
		Name:            "Backups: Content",
		DefaultValue:    false,
		RequiresConsent: false,
		RequiresAdmin:   false,
		RiskLevel:       RiskLevelMedium,
		Description:     "Enables content backups (photos and app data) for managed stacks.",
		Category:        CategoryBeta,
	},
	// The pinned release enforces local Photos/Documents audiences. Other
	// explicit choices retain the structured use-case-access refusal.
	"use_case_access": {
		Key:             edgeFlagTechStackUseCaseAccess,
		Name:            "Access per use case",
		DefaultValue:    true,
		RequiresConsent: false,
		RequiresAdmin:   false,
		RiskLevel:       RiskLevelLow,
		Description:     "Lets the creation flow ask where one use case is reachable and who gets it.",
		Category:        CategoryBeta,
	},
	// A restore over live data and an Advanced change-set apply run only on
	// the platform's signed decision receipt instead of a self-asserted
	// owner_approved (platform-8f0n2.20). Off until the Gateway part and the
	// confirmation cards are live; off behaves exactly as before.
	"stackkit_decision_receipt": {
		Key:             edgeFlagTechStackDecisionReceipt,
		Name:            "Signed confirmation for destructive operations",
		DefaultValue:    false,
		RequiresConsent: false,
		RequiresAdmin:   false,
		RiskLevel:       RiskLevelLow,
		Description:     "Runs a restore over live data or an Advanced change set only after the Owner's signed confirmation.",
		Category:        CategoryBeta,
	},
}

// Service resolves the shipped feature catalog and enforces consent.
type Service struct {
	store       Store
	definitions map[string]FeatureDefinition
	edition     config.Edition
	log         *logger.Logger
}

type ServiceConfig struct {
	Edition config.Edition
}

const (
	edgeFlagTechStackManagedRuntime         = "techstack.managed.runtime"
	edgeFlagTechStackManagedRuntimeCloudKit = "techstack.managed.runtime.cloudkit"
	edgeFlagTechStackManagedRuntimeBaseKit  = "techstack.managed.runtime.basekit"
	edgeFlagTechStackManagedRuntimeCentron  = "techstack.managed.runtime.centron"
	edgeFlagTechStackManagedRuntimeIONOS    = "techstack.managed.runtime.ionos"
	edgeFlagTechStackBackupConfig           = "techstack.backup.config"
	edgeFlagTechStackBackupContent          = "techstack.backup.content"
	edgeFlagTechStackUseCaseAccess          = "techstack.use-case-access"
	edgeFlagTechStackDecisionReceipt        = "techstack.stackkit-decision-receipt"
)

var featureDefinitionAliases = map[string]string{
	edgeFlagTechStackManagedRuntime:                     "monthly_runtime",
	"techstack.monthly.runtime":                         "monthly_runtime",
	edgeFlagTechStackManagedRuntimeCloudKit:             "monthly_runtime_cloudkit",
	"techstack.monthly.runtime.cloudkit":                "monthly_runtime_cloudkit",
	edgeFlagTechStackManagedRuntimeBaseKit:              "monthly_runtime_cloudkit",
	"techstack.monthly.runtime.basekit":                 "monthly_runtime_cloudkit",
	serverruntime.FeatureTechStackMonthlyRuntimeBaseKit: "monthly_runtime_cloudkit",
	"monthly_runtime_basekit":                           "monthly_runtime_cloudkit",
	edgeFlagTechStackManagedRuntimeCentron:              "monthly_runtime_centron",
	"techstack.monthly.runtime.centron":                 "monthly_runtime_centron",
	edgeFlagTechStackManagedRuntimeIONOS:                "monthly_runtime_ionos",
	"techstack.monthly.runtime.ionos":                   "monthly_runtime_ionos",
}

// NewService creates a new feature flag service
func NewService(store Store, cfg ServiceConfig) (*Service, error) {
	log := logger.Get().WithComponent("features")

	// Merge all feature definitions
	definitions := make(map[string]FeatureDefinition)
	for k, v := range securityFeatures {
		definitions[k] = v
	}
	for k, v := range betaFeatures {
		definitions[k] = v
	}

	svc := &Service{
		store:       store,
		definitions: definitions,
		edition:     cfg.Edition,
		log:         log,
	}

	log.Info("feature flag service initialized",
		"definitions", len(definitions),
		"security_features", len(securityFeatures),
		"beta_features", len(betaFeatures),
		"edge_flags", "enabled",
	)

	return svc, nil
}

// IsEnabled checks if a feature is enabled for a user
func (s *Service) IsEnabled(ctx context.Context, featureKey string, userID string) (bool, error) {
	localKey, def, exists := s.resolveFeature(featureKey)
	if !exists {
		return false, fmt.Errorf("unknown feature: %s", featureKey)
	}
	value, _ := s.flagDecision(ctx, def)
	if !value || !def.RequiresConsent || def.DefaultValue {
		return value, nil
	}
	if s.store == nil {
		return false, nil
	}
	consent, err := s.store.HasUserConsent(ctx, userID, localKey)
	if err != nil {
		return false, fmt.Errorf("read feature consent: %w", err)
	}
	return consent, nil
}

// flagDecision returns whether the feature is enabled and centrally controlled.
func (s *Service) flagDecision(ctx context.Context, def FeatureDefinition) (bool, bool) {
	if flags, present := edgeauth.FlagsFromContext(ctx); present {
		return flags.Flags[signedFlagKey(def.Key)], true
	}
	if s.edition != "" && s.edition.IsSaaS() {
		return false, true
	}
	// Provider purchasing variants remain off without a centrally supplied
	// decision, including in the self-host edition.
	if def.Key == "ionos_standard_photo_memory" {
		return false, false
	}
	if s.edition == config.EditionSelfHostOSS {
		return true, false
	}
	return def.DefaultValue, false
}

// Gateway normalizes underscores to dots and signs monthly-runtime aliases
// under the canonical managed-runtime key (flagship.ts FLAG_KEY_ALIASES).
func signedFlagKey(key string) string {
	switch key {
	case serverruntime.FeatureTechStackMonthlyRuntime:
		return edgeFlagTechStackManagedRuntime
	case serverruntime.FeatureTechStackMonthlyRuntimeCentron:
		return edgeFlagTechStackManagedRuntimeCentron
	case serverruntime.FeatureTechStackMonthlyRuntimeIONOS:
		return edgeFlagTechStackManagedRuntimeIONOS
	default:
		return strings.ReplaceAll(key, "_", ".")
	}
}

// GetDefinition returns the definition for a feature
func (s *Service) GetDefinition(featureKey string) (*FeatureDefinition, bool) {
	_, def, exists := s.resolveFeature(featureKey)
	if !exists {
		return nil, false
	}
	return &def, true
}

func (s *Service) resolveFeature(featureKey string) (string, FeatureDefinition, bool) {
	key := strings.TrimSpace(featureKey)
	if def, exists := s.definitions[key]; exists {
		return key, def, true
	}
	if localKey, exists := featureDefinitionAliases[key]; exists {
		if def, ok := s.definitions[localKey]; ok {
			return localKey, def, true
		}
	}
	for localKey, def := range s.definitions {
		if strings.TrimSpace(def.Key) == key {
			return localKey, def, true
		}
	}
	return key, FeatureDefinition{}, false
}

// GetAllFlags lists effective decisions with a single batch consent read.
func (s *Service) GetAllFlags(ctx context.Context, userID string, isInstanceOwner bool) (map[string]FlagState, error) {
	result := make(map[string]FlagState)

	// Collect all feature keys that need consent checking
	var consentRequiredKeys []string
	for key, def := range s.definitions {
		if def.RequiresConsent {
			consentRequiredKeys = append(consentRequiredKeys, key)
		}
	}

	// Batch fetch consent independently of the flag source.
	var userConsents map[string]bool

	if s.store != nil {
		var err error
		if len(consentRequiredKeys) > 0 {
			userConsents, err = s.store.GetUserConsentsMap(ctx, userID, consentRequiredKeys)
			if err != nil {
				return nil, fmt.Errorf("read feature consents: %w", err)
			}
		} else {
			userConsents = make(map[string]bool)
		}
	}

	// Build flag states using prefetched data
	for key, def := range s.definitions {
		enabled, lockedByEdge := s.flagDecision(ctx, def)
		hasConsent := userConsents[key]
		if def.RequiresConsent && enabled && !def.DefaultValue && !hasConsent {
			enabled = false
		}

		result[key] = FlagState{
			Key:             key,
			Name:            def.Name,
			Enabled:         enabled,
			Locked:          lockedByEdge || (def.RequiresAdmin && !isInstanceOwner),
			RequiresConsent: def.RequiresConsent,
			HasConsent:      hasConsent,
			RiskLevel:       def.RiskLevel,
			Description:     def.Description,
			Category:        def.Category,
		}
	}

	return result, nil
}

// GetFlagsByCategory returns flags grouped by category
func (s *Service) GetFlagsByCategory(ctx context.Context, userID string, isInstanceOwner bool) (map[Category][]FlagState, error) {
	allFlags, err := s.GetAllFlags(ctx, userID, isInstanceOwner)
	if err != nil {
		return nil, err
	}

	result := map[Category][]FlagState{
		CategorySecurity: {},
		CategoryBeta:     {},
		CategoryUX:       {},
	}

	for _, state := range allFlags {
		result[state.Category] = append(result[state.Category], state)
	}

	return result, nil
}
