package routes

import (
	"context"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/kombifyio/homelab/pkg/appcatalog"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/serviceregistry"
	"github.com/kombifyio/homelab/pkg/specv2"
)

// DetectedExistingApp is one observed application discovered on an existing
// enrolled server and classified through the 3-tier recognition spectrum for
// the creation wizard.
type DetectedExistingApp struct {
	ServiceID         string                  `json:"service_id"`
	AppID             string                  `json:"app_id"`
	DisplayName       string                  `json:"display_name"`
	SpectrumTier      appcatalog.SpectrumTier `json:"spectrum_tier"`
	UseCaseSlug       string                  `json:"use_case_slug,omitempty"`
	FirstClassPackage string                  `json:"first_class_package,omitempty"`
	AdoptionMode      string                  `json:"adoption_mode"`
	ContainerID       string                  `json:"container_id,omitempty"`
	ComposeProject    string                  `json:"compose_project,omitempty"`
	IconURL           string                  `json:"icon_url,omitempty"`
	Logo              *appcatalog.Logo        `json:"logo,omitempty"`
	VendorName        string                  `json:"vendor_name,omitempty"`
	DocsURL           string                  `json:"docs_url,omitempty"`
	HomepageURL       string                  `json:"homepage_url,omitempty"`
	Ports             []int                   `json:"ports,omitempty"`
}

// detectExistingServerApps queries observed services on an enrolled existing
// server owned by ownerID in tenantID, classifies them through appcatalog's
// 3-tier spectrum and synonym table, and returns a deduplicated, deterministic
// list for wizard takeover proposals.
func detectExistingServerApps(
	ctx context.Context,
	serverStore controlplane.ServerRuntimeStore,
	serviceStore controlplane.ServiceRuntimeStore,
	tenantID, ownerID, serverID string,
) []DetectedExistingApp {
	serverID = strings.TrimSpace(serverID)
	if serverID == "" || serviceStore == nil {
		return nil
	}
	if serverStore != nil {
		server, err := serverStore.GetServerRuntime(ctx, tenantID, serverID)
		if err != nil || server == nil {
			return nil
		}
		if strings.TrimSpace(server.TenantID) != strings.TrimSpace(tenantID) ||
			strings.TrimSpace(server.OwnerSubjectID) != strings.TrimSpace(ownerID) {
			return nil
		}
	}
	rows, err := serviceStore.ListServiceRuntimes(ctx, tenantID, "", serverID)
	if err != nil || len(rows) == 0 {
		return nil
	}

	type dedupeKey struct {
		appID          string
		composeProject string
	}
	byKey := make(map[dedupeKey]int)
	var detected []DetectedExistingApp

	for _, svc := range rows {
		if serviceregistry.CanonicalManagementState(svc.ManagementState) != serviceregistry.ManagementObserved {
			continue
		}
		subject := serviceSubject(svc)
		grouping := resolveServiceApplication(svc)
		if grouping.Key == appcatalog.SystemKey {
			continue
		}
		entry := serviceCatalogEntry(svc)
		if entry == nil || entry.Infra || entry.Category == "database" {
			continue
		}

		appID := entry.ID
		if appID == "custom" || appID == "" {
			appID = firstNonEmptyString(grouping.Key, appcatalog.ContainerKey(svc.Name), svc.ServiceKey)
		}
		displayName := firstNonEmptyString(entry.DisplayName, grouping.DisplayName, svc.Name, appID)
		if entry.SpectrumTier == appcatalog.SpectrumTierCustomApp {
			displayName = firstNonEmptyString(grouping.DisplayName, svc.Name, appID)
		}

		ports := extractServicePorts(svc, entry)
		key := dedupeKey{
			appID:          appID,
			composeProject: strings.ToLower(strings.TrimSpace(subject.ComposeProject)),
		}
		if idx, exists := byKey[key]; exists {
			detected[idx].Ports = mergeUniquePorts(detected[idx].Ports, ports)
			continue
		}

		item := DetectedExistingApp{
			ServiceID:         svc.ID,
			AppID:             appID,
			DisplayName:       displayName,
			SpectrumTier:      entry.SpectrumTier,
			UseCaseSlug:       entry.UseCaseSlug,
			FirstClassPackage: entry.FirstClassPackage,
			AdoptionMode:      entry.AdoptionMode,
			ContainerID:       strings.TrimSpace(stringFromAnyMap(svc.Metadata, "container_id")),
			ComposeProject:    strings.TrimSpace(subject.ComposeProject),
			IconURL:           entry.IconURL,
			Logo:              entry.Logo,
			VendorName:        entry.VendorName,
			DocsURL:           entry.DocsURL,
			HomepageURL:       entry.HomepageURL,
			Ports:             ports,
		}
		byKey[key] = len(detected)
		detected = append(detected, item)
	}

	sort.Slice(detected, func(i, j int) bool {
		ti, tj := spectrumTierOrder(detected[i].SpectrumTier), spectrumTierOrder(detected[j].SpectrumTier)
		if ti != tj {
			return ti < tj
		}
		if detected[i].DisplayName != detected[j].DisplayName {
			return strings.ToLower(detected[i].DisplayName) < strings.ToLower(detected[j].DisplayName)
		}
		return detected[i].ServiceID < detected[j].ServiceID
	})
	return detected
}

func spectrumTierOrder(tier appcatalog.SpectrumTier) int {
	switch tier {
	case appcatalog.SpectrumTierFirstClass:
		return 0
	case appcatalog.SpectrumTierKnownCatalog:
		return 1
	default:
		return 2
	}
}

func hasDetectedSmartHome(apps []DetectedExistingApp) bool {
	for _, app := range apps {
		if app.UseCaseSlug == "smart-home" || app.AppID == "home-assistant" {
			return true
		}
	}
	return false
}

// applyDetectedSmartHomeContext automatically proposes non-destructive
// observation/takeover when Home Assistant is detected on the selected
// existing server and the user has not explicitly forced a fresh install.
func applyDetectedSmartHomeContext(
	ctx specv2.SmartHomeContext,
	settings map[string]any,
	apps []DetectedExistingApp,
) specv2.SmartHomeContext {
	if !hasDetectedSmartHome(apps) {
		return ctx
	}
	if origin, _ := settings["instance-origin"].(string); strings.EqualFold(strings.TrimSpace(origin), "new") {
		return ctx
	}
	ctx.Existing = true
	ctx.AutoDetected = true
	return ctx
}

func extractServicePorts(svc controlplane.ServiceRuntime, entry *appcatalog.Entry) []int {
	var ports []int
	if rawEndpoints, ok := svc.Metadata[workerInventoryEndpointsKey]; ok {
		ports = appendPortsFromEndpoints(ports, rawEndpoints)
	}
	if svc.Access != nil {
		if rawURL, ok := svc.Access["url"].(string); ok {
			if p := portFromURL(rawURL); p > 0 {
				ports = append(ports, p)
			}
		}
	}
	if len(ports) == 0 && entry != nil && entry.WebPort > 0 {
		ports = append(ports, entry.WebPort)
	}
	return mergeUniquePorts(nil, ports)
}

func appendPortsFromEndpoints(dst []int, raw any) []int {
	switch items := raw.(type) {
	case []map[string]any:
		for _, item := range items {
			dst = appendPortFromMap(dst, item)
		}
	case []any:
		for _, item := range items {
			if m, ok := item.(map[string]any); ok {
				dst = appendPortFromMap(dst, m)
			}
		}
	}
	return dst
}

func appendPortFromMap(dst []int, m map[string]any) []int {
	for _, key := range []string{"port", "host_port", "container_port"} {
		switch value := m[key].(type) {
		case int:
			if value > 0 && value <= 65535 {
				dst = append(dst, value)
			}
		case int64:
			if value > 0 && value <= 65535 {
				dst = append(dst, int(value))
			}
		case float64:
			if value > 0 && value <= 65535 {
				dst = append(dst, int(value))
			}
		}
	}
	if rawURL, ok := m["url"].(string); ok {
		if p := portFromURL(rawURL); p > 0 {
			dst = append(dst, p)
		}
	}
	return dst
}

func portFromURL(raw string) int {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Port() == "" {
		return 0
	}
	p, err := strconv.Atoi(parsed.Port())
	if err != nil || p <= 0 || p > 65535 {
		return 0
	}
	return p
}

func mergeUniquePorts(existing, incoming []int) []int {
	if len(existing) == 0 && len(incoming) == 0 {
		return nil
	}
	merged := make([]int, 0, len(existing)+len(incoming))
	for _, p := range existing {
		if p > 0 && !slices.Contains(merged, p) {
			merged = append(merged, p)
		}
	}
	for _, p := range incoming {
		if p > 0 && !slices.Contains(merged, p) {
			merged = append(merged, p)
		}
	}
	sort.Ints(merged)
	return merged
}
