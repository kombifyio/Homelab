package appcatalog

import (
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/kombifyio/homelab/internal/stackkitrelease"
)

var pinnedApplicationCatalog = sync.OnceValues(stackkitrelease.PinnedUseCaseCatalog)
var pinnedApplicationMetadata = sync.OnceValues(func() (stackkitrelease.ApplicationMetadataCatalog, error) {
	metadata, err := stackkitrelease.PinnedApplicationMetadata()
	if err != nil {
		slog.Error("published SDK application metadata is invalid", "error", err)
	}
	return metadata, err
})

var pinnedApplications = sync.OnceValue(func() map[string]Entry {
	catalog, err := pinnedApplicationCatalog()
	if err != nil {
		return nil
	}
	return applicationsFromRelease(catalog)
})

// releasedPresentations returns the pinned snapshot's application presentation
// keyed by catalog application id, or nil when this deployment's catalog is not
// the snapshot's own release. The snapshot describes applications no use case
// lists (the Node Hub, Dokploy, Komodo), so every consumer joins on this map
// instead of only on use-case components.
func releasedPresentations() map[string]stackkitrelease.ApplicationPresentation {
	metadata, err := pinnedApplicationMetadata()
	if err != nil {
		return nil
	}
	catalog, err := pinnedApplicationCatalog()
	if strings.TrimSpace(os.Getenv(stackkitrelease.UseCaseCatalogEnv)) != "" {
		catalog, _, err = stackkitrelease.UseCaseCatalogFromEnv()
	}
	if err != nil || !metadata.Source.MatchesCatalog(catalog.Release) {
		return nil
	}
	presentations := make(map[string]stackkitrelease.ApplicationPresentation, len(metadata.Applications))
	for _, presentation := range metadata.Applications {
		presentations[presentation.ID] = presentation
	}
	return presentations
}

// Runtime recognition retains image/name hints for custom installations. The
// exact released application ID joins presentation facts from the same catalog
// the wizard reads; recognition never grants installation availability.
func releasedCatalogEntry(id string) *Entry {
	if strings.TrimSpace(os.Getenv(stackkitrelease.UseCaseCatalogEnv)) != "" {
		catalog, _, err := stackkitrelease.UseCaseCatalogFromEnv()
		if err != nil {
			return nil
		}
		for _, useCase := range catalog.UseCases {
			for _, component := range useCase.Components {
				if component.Kind == "application" && component.ID == id {
					entry := &Entry{ID: id, DisplayName: component.Name, Category: useCase.ID}
					attachReleasedApplicationPresentation(entry)
					attachPresentation(entry)
					attachSpectrum(entry)
					return entry
				}
			}
		}
		return nil
	}
	entry, ok := pinnedApplications()[id]
	if !ok {
		entry, ok = releasedSnapshotEntry(id)
		if !ok {
			return nil
		}
	}
	attachReleasedApplicationPresentation(&entry)
	attachPresentation(&entry)
	attachSpectrum(&entry)
	return &entry
}

// releasedSnapshotEntry serves an application the pinned snapshot describes
// but no use case lists. Its id is still the catalog application id, so
// recognition by that name resolves the same guide, clients and vendor facts
// as an application a use case brings; nothing grants installation support.
func releasedSnapshotEntry(id string) (Entry, bool) {
	if _, ok := releasedPresentations()[id]; !ok {
		return Entry{}, false
	}
	displayName, category := titleFromSlug(id), defaultCategory
	if generated := loadGeneratedTable().apps[id]; generated != nil {
		displayName = firstNonEmpty(generated.Name, displayName)
		category = firstNonEmpty(generated.Category, category)
	}
	return Entry{ID: id, DisplayName: displayName, Category: category}, true
}

// Display links follow the same public source as the use-case catalog. A
// development catalog override cannot borrow another pin's client/vendor data.
func attachReleasedApplicationPresentation(entry *Entry) {
	entry.GuideURL = ""
	entry.Clients = nil
	entry.Vendor = nil
	presentation, ok := presentationForEntry(entry, releasedPresentations())
	if !ok {
		return
	}
	entry.GuideURL = presentation.GuideURL
	entry.Clients = append([]stackkitrelease.ApplicationClient(nil), presentation.Clients...)
	if presentation.Vendor != nil {
		vendor := *presentation.Vendor
		vendor.Support = append([]stackkitrelease.ApplicationSupportLink(nil), presentation.Vendor.Support...)
		entry.Vendor = &vendor
	}
}

// presentationForEntry joins on the catalog application id. Homelab's hand
// catalog still carries a few historical ids (pocket-id for pocketid); the
// catalog id wins through the names the entry already declares, and an
// ambiguous match attaches nothing.
func presentationForEntry(entry *Entry, presentations map[string]stackkitrelease.ApplicationPresentation) (stackkitrelease.ApplicationPresentation, bool) {
	if entry == nil || presentations == nil {
		return stackkitrelease.ApplicationPresentation{}, false
	}
	if presentation, ok := presentations[entry.ID]; ok {
		return presentation, true
	}
	if len(entry.names) == 0 {
		return stackkitrelease.ApplicationPresentation{}, false
	}
	var matched stackkitrelease.ApplicationPresentation
	matchedID := ""
	for id, presentation := range presentations {
		alias := slugify(id)
		if alias == "" {
			continue
		}
		for _, name := range entry.names {
			if slugify(name) != alias {
				continue
			}
			if matchedID != "" && matchedID != id {
				return stackkitrelease.ApplicationPresentation{}, false
			}
			matched, matchedID = presentation, id
			break
		}
	}
	return matched, matchedID != ""
}

func applicationsFromRelease(catalog stackkitrelease.UseCaseCatalog) map[string]Entry {
	applications := map[string]Entry{}
	for _, useCase := range catalog.UseCases {
		for _, component := range useCase.Components {
			if component.Kind != "application" {
				continue
			}
			if _, present := applications[component.ID]; present {
				continue
			}
			applications[component.ID] = Entry{
				ID: component.ID, DisplayName: component.Name,
				Category: useCase.ID,
			}
		}
	}
	return applications
}
