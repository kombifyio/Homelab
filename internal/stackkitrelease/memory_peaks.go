package stackkitrelease

import (
	_ "embed"
	"encoding/json"
	"math"
)

// Generated from evidence at the pinned catalog source; generation is offline
// from the runtime and the catalog's exact bytes must match.
//
//go:embed pinned-memory-peaks.json
var pinnedMemoryPeaksJSON []byte

var pinnedMemoryPeaks = func() struct {
	SourceSHA     string `json:"sourceSha"`
	CatalogSHA256 string `json:"catalogSha256"`
	PeaksMiB      map[string]struct {
		PeakMiB        float64 `json:"peakMiB"`
		EvidenceSHA256 string  `json:"evidenceSha256"`
	} `json:"peaksMiB"`
} {
	var artifact struct {
		SourceSHA     string `json:"sourceSha"`
		CatalogSHA256 string `json:"catalogSha256"`
		PeaksMiB      map[string]struct {
			PeakMiB        float64 `json:"peakMiB"`
			EvidenceSHA256 string  `json:"evidenceSha256"`
		} `json:"peaksMiB"`
	}
	if json.Unmarshal(pinnedMemoryPeaksJSON, &artifact) != nil {
		return artifact
	}
	return artifact
}()

// MeasuredMemoryPeak returns a scoped module observation only for the exact
// catalog and matching measured provenance. Missing evidence remains unknown.
func MeasuredMemoryPeak(catalog UseCaseCatalog, footprint WorkloadFootprint) (float64, bool) {
	if catalog.Release.SourceSHA == "" || catalog.Release.SourceSHA != pinnedMemoryPeaks.SourceSHA || catalog.RawSHA256 == "" || catalog.RawSHA256 != pinnedMemoryPeaks.CatalogSHA256 {
		return 0, false
	}
	key := footprint.Module + "/" + footprint.ComputeProfile
	ref := "docs/data/resource-evidence/" + key + ".json"
	if footprint.RecommendedProvenance.Source != "measured" || footprint.RecommendedProvenance.Ref != ref {
		return 0, false
	}
	entry, ok := pinnedMemoryPeaks.PeaksMiB[key]
	if !ok || len(entry.EvidenceSHA256) != 64 || entry.PeakMiB <= 0 || math.IsNaN(entry.PeakMiB) || math.IsInf(entry.PeakMiB, 0) {
		return 0, false
	}
	return entry.PeakMiB, true
}
