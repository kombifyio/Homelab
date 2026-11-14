package providercontrol

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kombifyio/techstack/internal/providercatalog"
)

// This file is the provider adapter contract outside the lifecycle verbs. The
// verbs a managed-server adapter implements are AtMostOnceProvisionExecutor,
// ExactHandleReconcileExecutor (power), ProvisionResolutionProvider (operator
// reconcile of an ambiguous create), ProviderCreateRegionResolver,
// ManagedProviderSafetyReporter and ManagedCredentialAuthority. What the adapter supplies about its provider is an
// AdapterDescriptor. Code outside the adapter reads provider facts only from
// here, never from a provider name. docs/ADD-A-PROVIDER.md is the prose form.

// AdapterCapability names an optional provider capability. An adapter declares
// only what its provider API can do and the adapter implements; a missing
// capability selects the standard path and never makes the provider unusable.
type AdapterCapability string

const (
	// CapabilityPrivateImageImport means the provider can import, publish and
	// select a private image, so an admitted pre-baked Guard image can be
	// booted instead of a catalogue image plus first-boot convergence.
	CapabilityPrivateImageImport AdapterCapability = "private_image_import"
	// CapabilityProviderPowerStop means the provider API stops a server while
	// keeping its allocation. Without it, stop powers the guest off over the
	// execution channel.
	CapabilityProviderPowerStop AdapterCapability = "provider_power_stop"
)

var knownAdapterCapabilities = map[AdapterCapability]struct{}{
	CapabilityPrivateImageImport: {},
	CapabilityProviderPowerStop:  {},
}

// ImageStrategy is how a managed server reaches the Guard and host state.
type ImageStrategy string

const (
	// ImageStrategyCatalogueFirstBoot is the standard path for every provider:
	// a catalogue OS image of the provider plus first-boot convergence
	// (guardbootstrap cloud-init and the served installer) to the same Guard
	// and host state the pre-baked image carries.
	ImageStrategyCatalogueFirstBoot ImageStrategy = "catalogue_first_boot"
	// ImageStrategyPrebaked boots an admitted pre-baked Guard image. It is an
	// optimisation, chosen only when the adapter declares
	// CapabilityPrivateImageImport and an admitted image is named.
	ImageStrategyPrebaked ImageStrategy = "prebaked"
)

// AdapterDescriptor is everything an adapter supplies about its provider
// besides the lifecycle verbs: identity, API base, credential custody, the
// catalogue image of the standard path, the default region, the one network
// detail the normalized bootstrap requirements cannot express, and its
// optional capabilities. Offering sizes and regions per offering are the
// provider's rows in pkg/monthlyruntime (the product's package table).
type AdapterDescriptor struct {
	ProviderID string
	AdapterID  string
	// APIBase is the provider API origin and path prefix the adapter calls.
	APIBase string
	// CredentialEnvironment names the one custody object holding the
	// provider credential; CredentialFields are its non-optional fields.
	CredentialEnvironment string
	CredentialFields      []string
	// CatalogueImage is the provider's catalogue OS image of the standard
	// path: an exact identifier, or the catalogue selector the adapter
	// resolves read-only before each create.
	CatalogueImage string
	// DefaultRegion is the region used when neither the offering nor the
	// request names one.
	DefaultRegion string
	// BootstrapNetworkException is the provider's network detail that the
	// normalized ManagedBootstrapNetworkRequirements cannot express. Its value
	// is part of persisted execution profiles and never changes for a build.
	BootstrapNetworkException ManagedBootstrapAdapterException
	Capabilities              []AdapterCapability
	// ManagedSSHPrivateKey returns the backend-only rollout key from the
	// provider custody object.
	ManagedSSHPrivateKey func(now time.Time) (string, error)
}

// Has reports whether the adapter declares the optional capability.
func (d AdapterDescriptor) Has(capability AdapterCapability) bool {
	for _, declared := range d.Capabilities {
		if declared == capability {
			return true
		}
	}
	return false
}

// CapabilityStates lists every known optional capability with its state, for
// receipts and operator output.
func (d AdapterDescriptor) CapabilityStates() map[AdapterCapability]string {
	states := make(map[AdapterCapability]string, len(knownAdapterCapabilities))
	for capability := range knownAdapterCapabilities {
		states[capability] = "unavailable"
		if d.Has(capability) {
			states[capability] = "available"
		}
	}
	return states
}

// Validate refuses an incomplete descriptor before it can register.
func (d AdapterDescriptor) Validate() error {
	if !providercatalog.IsCanonicalProviderID(d.ProviderID) {
		return fmt.Errorf("providercontrol: adapter descriptor provider %q is not a canonical provider identity", d.ProviderID)
	}
	for _, field := range []struct{ name, value string }{
		{"adapter_id", d.AdapterID}, {"api_base", d.APIBase},
		{"credential_environment", d.CredentialEnvironment}, {"catalogue_image", d.CatalogueImage},
		{"default_region", d.DefaultRegion}, {"bootstrap_network_exception", string(d.BootstrapNetworkException)},
	} {
		if strings.TrimSpace(field.value) == "" || strings.TrimSpace(field.value) != field.value {
			return fmt.Errorf("providercontrol: adapter descriptor %s/%s has no exact %s", d.ProviderID, d.AdapterID, field.name)
		}
	}
	if len(d.CredentialFields) == 0 || d.ManagedSSHPrivateKey == nil {
		return fmt.Errorf("providercontrol: adapter descriptor %s/%s has no credential contract", d.ProviderID, d.AdapterID)
	}
	for _, capability := range d.Capabilities {
		if _, known := knownAdapterCapabilities[capability]; !known {
			return fmt.Errorf("providercontrol: adapter descriptor %s/%s declares unknown capability %q", d.ProviderID, d.AdapterID, capability)
		}
	}
	return nil
}

var (
	adapterDescriptorsMu sync.RWMutex
	adapterDescriptors   = map[string]AdapterDescriptor{}
)

func adapterDescriptorKey(providerID, adapterID string) string {
	return strings.TrimSpace(providerID) + "\x00" + strings.TrimSpace(adapterID)
}

// RegisterAdapterDescriptor is called once from each adapter package's init.
// An invalid or duplicate descriptor is a build defect and panics at start.
func RegisterAdapterDescriptor(descriptor AdapterDescriptor) {
	if err := descriptor.Validate(); err != nil {
		panic(err)
	}
	adapterDescriptorsMu.Lock()
	defer adapterDescriptorsMu.Unlock()
	key := adapterDescriptorKey(descriptor.ProviderID, descriptor.AdapterID)
	if _, exists := adapterDescriptors[key]; exists {
		panic(fmt.Sprintf("providercontrol: adapter descriptor %s/%s registered twice", descriptor.ProviderID, descriptor.AdapterID))
	}
	descriptor.CredentialFields = append([]string(nil), descriptor.CredentialFields...)
	descriptor.Capabilities = append([]AdapterCapability(nil), descriptor.Capabilities...)
	adapterDescriptors[key] = descriptor
}

// AdapterDescriptorFor returns the registered descriptor of one exact
// provider/adapter pair.
func AdapterDescriptorFor(providerID, adapterID string) (AdapterDescriptor, bool) {
	adapterDescriptorsMu.RLock()
	defer adapterDescriptorsMu.RUnlock()
	descriptor, ok := adapterDescriptors[adapterDescriptorKey(providerID, adapterID)]
	return descriptor, ok
}

// AdapterDescriptorForProvider returns the single registered descriptor of a
// provider. Two adapters of one provider make the lookup ambiguous.
func AdapterDescriptorForProvider(providerID string) (AdapterDescriptor, bool) {
	adapterDescriptorsMu.RLock()
	defer adapterDescriptorsMu.RUnlock()
	var found AdapterDescriptor
	count := 0
	for _, descriptor := range adapterDescriptors {
		if descriptor.ProviderID == strings.TrimSpace(providerID) {
			found = descriptor
			count++
		}
	}
	return found, count == 1
}

// RegisteredAdapterDescriptors lists every registered descriptor in a stable
// order.
func RegisteredAdapterDescriptors() []AdapterDescriptor {
	adapterDescriptorsMu.RLock()
	defer adapterDescriptorsMu.RUnlock()
	out := make([]AdapterDescriptor, 0, len(adapterDescriptors))
	for _, descriptor := range adapterDescriptors {
		out = append(out, descriptor)
	}
	sort.Slice(out, func(i, j int) bool {
		return adapterDescriptorKey(out[i].ProviderID, out[i].AdapterID) < adapterDescriptorKey(out[j].ProviderID, out[j].AdapterID)
	})
	return out
}

// SelectImageStrategy is the one image decision for every adapter: the
// standard path unless the adapter declares private image import and an
// admitted pre-baked image is named for this create.
func SelectImageStrategy(descriptor AdapterDescriptor, admittedImageNamed bool) ImageStrategy {
	if admittedImageNamed && descriptor.Has(CapabilityPrivateImageImport) {
		return ImageStrategyPrebaked
	}
	return ImageStrategyCatalogueFirstBoot
}

// ErrCapabilityUnavailable is the root of every missing optional capability.
// It never means the provider is unusable: the standard path still applies.
var ErrCapabilityUnavailable = errors.New("provider capability unavailable")

// CapabilityUnavailableError names the provider and the missing capability.
type CapabilityUnavailableError struct {
	ProviderID string
	Capability AdapterCapability
}

func (e *CapabilityUnavailableError) Error() string {
	return fmt.Sprintf("provider %s: optional capability %s is unavailable; managed servers use the standard path (%s)",
		e.ProviderID, e.Capability, ImageStrategyCatalogueFirstBoot)
}

func (e *CapabilityUnavailableError) Unwrap() error { return ErrCapabilityUnavailable }

// RequireCapability returns nil when the adapter declares the capability and
// a CapabilityUnavailableError otherwise.
func RequireCapability(descriptor AdapterDescriptor, capability AdapterCapability) error {
	if descriptor.Has(capability) {
		return nil
	}
	return &CapabilityUnavailableError{ProviderID: descriptor.ProviderID, Capability: capability}
}
